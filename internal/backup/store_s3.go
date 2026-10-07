package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"golang.org/x/sync/errgroup"
)

const (
	defaultPartSize = 16 << 20 // S3 multipart minimum is 5 MiB
	// After bigPartAfter parts the part size jumps so one object can exceed 160 GiB
	// without hitting S3's 10,000 part limit.
	bigPartAfter    = 1000
	bigPartSize     = 64 << 20
	uploadParallel  = 3
	deleteBatchSize = 1000
	s3Attempts      = 5
)

// S3Options configures NewS3Store.
type S3Options struct {
	Bucket string
	Prefix string // key prefix inside the bucket, no leading or trailing slash
	// Endpoint overrides the AWS endpoint, for MinIO, R2, Backblaze, Hetzner and so on.
	Endpoint       string
	Region         string
	ForcePathStyle bool
	// AccessKeyID and SecretKey are static credentials; empty means the default AWS chain.
	AccessKeyID string
	SecretKey   string
	// PartSize is the multipart part size; zero means 16 MiB. Tests lower it.
	PartSize int64
}

// S3Store keeps objects in an S3 bucket (or any S3-compatible service).
type S3Store struct {
	c        *s3.Client
	bucket   string
	prefix   string
	partSize int64
	// needMD5 is set once the service has refused DeleteObjects without Content-MD5.
	needMD5 atomic.Bool
}

// NewS3Store connects with the AWS SDK default credential chain, or static
// credentials when given. A custom endpoint without a region uses "us-east-1".
func NewS3Store(ctx context.Context, o S3Options) (*S3Store, error) {
	opts := []func(*config.LoadOptions) error{config.WithRetryMaxAttempts(s3Attempts)}
	region := o.Region
	if region == "" {
		region = firstEnv("AWS_REGION", "AWS_DEFAULT_REGION")
	}
	if region == "" && o.Endpoint != "" {
		region = "us-east-1"
	}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	if o.AccessKeyID != "" || o.SecretKey != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.SecretKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("backup: s3 config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("backup: s3 region unknown: set backup.s3_region or AWS_REGION")
	}
	c := s3.NewFromConfig(cfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.ForcePathStyle
		// Newer SDKs add CRC32 trailers by default, which many S3-compatible servers reject.
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	ps := o.PartSize
	if ps == 0 {
		ps = defaultPartSize
	}
	return &S3Store{c: c, bucket: o.Bucket, prefix: strings.Trim(o.Prefix, "/"), partSize: ps}, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func (s *S3Store) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

func (s *S3Store) rel(k string) string {
	if s.prefix == "" {
		return k
	}
	return strings.TrimPrefix(k, s.prefix+"/")
}

// URL implements Store.
func (s *S3Store) URL(key string) string { return "s3://" + s.bucket + "/" + s.key(key) }

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

// accessDeniedHint explains the usual cause of a 403 on a key lookup. Without
// s3:ListBucket, S3 answers 403 instead of 404 for a key that does not exist, so a
// first-time wal push or the end of a wal fetch looks like a permission failure.
func accessDeniedHint(err error) string {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusForbidden {
		return " (HTTP 403: the credentials need s3:ListBucket on the bucket as well as s3:GetObject, otherwise S3 answers 403 for a missing key)"
	}
	return ""
}

// Put implements Store. Objects up to one part are a single PutObject; larger streams
// use a multipart upload that is aborted on failure, so no partial object is visible.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader) error {
	if err := validKey(key); err != nil {
		return err
	}
	k := s.key(key)
	r = ctxReader{ctx, r}
	var first bytes.Buffer
	n, err := first.ReadFrom(io.LimitReader(r, s.partSize))
	if err != nil {
		return err
	}
	br := bufio.NewReader(r)
	if n < s.partSize {
		return s.putSmall(ctx, k, first.Bytes())
	}
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return s.putSmall(ctx, k, first.Bytes())
	} else if err != nil {
		return err
	}
	return s.putMultipart(ctx, k, first.Bytes(), br)
}

func (s *S3Store) putSmall(ctx context.Context, k string, b []byte) error {
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &k, Body: bytes.NewReader(b), ContentLength: aws.Int64(int64(len(b))),
	})
	if err != nil {
		return fmt.Errorf("backup: s3 put %s: %w", k, err)
	}
	return nil
}

func (s *S3Store) putMultipart(ctx context.Context, k string, first []byte, rest io.Reader) (err error) {
	up, err := s.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		return fmt.Errorf("backup: s3 create multipart %s: %w", k, err)
	}
	defer func() {
		if err != nil {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			s.c.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &k, UploadId: up.UploadId})
		}
	}()

	var (
		mu    sync.Mutex
		parts []types.CompletedPart
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	send := func(num int32, body []byte) {
		g.Go(func() error {
			out, err := s.c.UploadPart(gctx, &s3.UploadPartInput{
				Bucket: &s.bucket, Key: &k, UploadId: up.UploadId, PartNumber: aws.Int32(num),
				Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
			})
			if err != nil {
				return fmt.Errorf("backup: s3 upload part %d of %s: %w", num, k, err)
			}
			mu.Lock()
			parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)})
			mu.Unlock()
			return nil
		})
	}

	num := int32(1)
	send(num, first)
	for {
		size := s.partSize
		if num >= bigPartAfter && size < bigPartSize {
			size = bigPartSize
		}
		var buf bytes.Buffer
		n, rerr := buf.ReadFrom(io.LimitReader(rest, size))
		if rerr != nil {
			err = rerr
			break
		}
		if n == 0 {
			break
		}
		num++
		send(num, buf.Bytes())
		if n < size {
			break
		}
		if gctx.Err() != nil {
			break
		}
	}
	if werr := g.Wait(); err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	sort.Slice(parts, func(i, j int) bool { return *parts[i].PartNumber < *parts[j].PartNumber })
	if _, err = s.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &s.bucket, Key: &k, UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		return fmt.Errorf("backup: s3 complete multipart %s: %w", k, err)
	}
	return nil
}

// Get implements Store.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	k := s.key(key)
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("backup: s3 get %s: %w%s", k, err, accessDeniedHint(err))
	}
	return out.Body, nil
}

// Stat implements Store.
func (s *S3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return ObjectInfo{}, err
	}
	k := s.key(key)
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("backup: s3 head %s: %w%s", k, err, accessDeniedHint(err))
	}
	return ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), ModTime: aws.ToTime(out.LastModified)}, nil
}

// List implements Store.
func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	p := s.key(prefix)
	if prefix == "" {
		p = s.prefix
		if p != "" {
			p += "/"
		}
	}
	var out []ObjectInfo
	pg := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &p})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup: s3 list %s: %w", p, err)
		}
		for _, o := range page.Contents {
			out = append(out, ObjectInfo{Key: s.rel(aws.ToString(o.Key)), Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

// ListDirs implements Store.
func (s *S3Store) ListDirs(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("backup: ListDirs prefix %q must end in /", prefix)
	}
	p := s.key(prefix)
	if prefix == "" && s.prefix != "" {
		p = s.prefix + "/"
	}
	delim := "/"
	var out []string
	pg := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &p, Delimiter: &delim})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup: s3 list %s: %w", p, err)
		}
		for _, cp := range page.CommonPrefixes {
			// Some S3-compatible stores (SeaweedFS) keep listing a "directory" after its
			// last object is deleted. Only report prefixes that still hold an object, so
			// restores and retention see the same tree as on AWS S3.
			full := aws.ToString(cp.Prefix)
			ok, err := s.hasObjects(ctx, full)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, strings.TrimSuffix(strings.TrimPrefix(full, p), "/"))
			}
		}
	}
	return out, nil
}

// hasObjects reports whether at least one object exists under the full key prefix.
func (s *S3Store) hasObjects(ctx context.Context, fullPrefix string) (bool, error) {
	one := int32(1)
	page, err := s.c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &fullPrefix, MaxKeys: &one})
	if err != nil {
		return false, fmt.Errorf("backup: s3 list %s: %w", fullPrefix, err)
	}
	return len(page.Contents) > 0, nil
}

// Delete implements Store.
func (s *S3Store) Delete(ctx context.Context, keys ...string) error {
	for len(keys) > 0 {
		n := min(len(keys), deleteBatchSize)
		ids := make([]types.ObjectIdentifier, n)
		for i, k := range keys[:n] {
			if err := validKey(k); err != nil {
				return err
			}
			ids[i] = types.ObjectIdentifier{Key: aws.String(s.key(k))}
		}
		out, err := s.deleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &s.bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("backup: s3 delete: %w", err)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("backup: s3 delete %s: %s: %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
		keys = keys[n:]
	}
	return nil
}

// deleteObjects sends DeleteObjects the way the SDK does (a CRC32 checksum header, which
// AWS accepts). Some S3-compatible services insist on Content-MD5 instead and answer
// with MissingContentMD5 or InvalidRequest; the first such answer switches this store to
// sending Content-MD5 and the request is repeated, so retention keeps working there.
func (s *S3Store) deleteObjects(ctx context.Context, in *s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error) {
	if s.needMD5.Load() {
		return s.c.DeleteObjects(ctx, in, withContentMD5)
	}
	out, err := s.c.DeleteObjects(ctx, in)
	if err != nil && wantsContentMD5(err) {
		s.needMD5.Store(true)
		return s.c.DeleteObjects(ctx, in, withContentMD5)
	}
	return out, err
}

// wantsContentMD5 reports whether err is a service's refusal of a DeleteObjects request
// that carries no Content-MD5 header.
func wantsContentMD5(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "MissingContentMD5", "InvalidRequest", "InvalidDigest":
		return true
	}
	return false
}

// withContentMD5 adds a Content-MD5 header over the serialized request body.
func withContentMD5(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Build.Add(contentMD5Middleware{}, middleware.After)
	})
}

type contentMD5Middleware struct{}

func (contentMD5Middleware) ID() string { return "sbctlContentMD5" }

func (contentMD5Middleware) HandleBuild(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (middleware.BuildOutput, middleware.Metadata, error) {
	req, ok := in.Request.(*smithyhttp.Request)
	if !ok || req.GetStream() == nil {
		return next.HandleBuild(ctx, in)
	}
	body, err := io.ReadAll(req.GetStream())
	if err != nil {
		return middleware.BuildOutput{}, middleware.Metadata{}, fmt.Errorf("backup: s3 content-md5: %w", err)
	}
	sum := md5.Sum(body)
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	if req, err = req.SetStream(bytes.NewReader(body)); err != nil {
		return middleware.BuildOutput{}, middleware.Metadata{}, err
	}
	in.Request = req
	return next.HandleBuild(ctx, in)
}
