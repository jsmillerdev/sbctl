package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/awsapi"
)

const (
	// singlePutMax is the largest object sent in one request. Bigger ones go in parts, each read
	// again from the file if a retry needs it.
	singlePutMax = 64 << 20
	partSizeMin  = 16 << 20
	// maxParts keeps one object under S3's limit of 10,000 parts.
	maxParts        = 9000
	partsInFlight   = 3
	deletesInFlight = 4
	s3Attempts      = 5
	// roleSession names the assumed-role session in CloudTrail.
	roleSession = "supavise-storage-migrate"
)

type s3Bucket struct {
	c         *s3.Client
	tr        *pacedTransport
	bucket    string
	threshold int64
	partSize  int64
}

// pace implements pacer.
func (b *s3Bucket) pace(l *limiter) { b.tr.lim.Store(l) }

// OpenS3 connects to the bucket with the AWS SDK. The credentials are the key of c, or the
// temporary credentials of the role it names, renewed before they expire.
func OpenS3(ctx context.Context, d Destination, c Credentials) (Bucket, error) {
	return openS3(ctx, d, c, stallAfter, s3Attempts)
}

func openS3(ctx context.Context, d Destination, c Credentials, stall time.Duration, attempts int) (Bucket, error) {
	if d.Bucket == "" {
		return nil, errors.New("storagemigrate: no bucket")
	}
	if d.Region == "" {
		return nil, errors.New("storagemigrate: no region for the bucket")
	}
	tr := &pacedTransport{next: awshttp.NewBuildableClient().GetTransport(), stall: stall}
	opts := []func(*config.LoadOptions) error{config.WithRegion(d.Region), config.WithRetryMaxAttempts(attempts),
		config.WithHTTPClient(&http.Client{Transport: tr})}
	switch c.Source {
	case CredFile, CredConfig:
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return nil, errors.New("storagemigrate: the access key is incomplete")
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, "")))
	case CredRole:
		p, err := roleProvider(c.RoleARN)
		if err != nil {
			return nil, err
		}
		opts = append(opts, config.WithCredentialsProvider(p))
	default:
		return nil, fmt.Errorf("storagemigrate: unknown credentials %q", c.Source)
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storagemigrate: s3 config: %w", err)
	}
	cl := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if d.Endpoint != "" {
			o.BaseEndpoint = aws.String(d.Endpoint)
		}
		o.UsePathStyle = d.PathStyle
		// Newer SDKs add CRC32 trailers by default, which many S3-compatible servers refuse.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &s3Bucket{c: cl, tr: tr, bucket: d.Bucket, threshold: singlePutMax, partSize: partSizeMin}, nil
}

// plainRemote says whether endpoint sends the objects and the signed requests unencrypted over a
// network: http to a host that is not this machine.
func plainRemote(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

// roleProvider assumes arn with the node's own credentials (the instance role on AWS) through
// internal/awsapi and hands the SDK the temporary credentials it gets.
func roleProvider(arn string) (aws.CredentialsProvider, error) {
	if arn == "" {
		return nil, errors.New("storagemigrate: no role to assume")
	}
	cl, err := awsapi.New(awsapi.Config{})
	if err != nil {
		return nil, err
	}
	src := cl.STS.RoleCredentials(awsapi.AssumeRoleInput{RoleARN: arn, SessionName: roleSession, Duration: time.Hour})
	return aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		c, err := src.Retrieve(ctx)
		if err != nil {
			return aws.Credentials{}, fmt.Errorf("assume %s: %w", arn, err)
		}
		return aws.Credentials{
			AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken,
			CanExpire: !c.Expires.IsZero(), Expires: c.Expires, Source: "supavise-awsapi",
		}, nil
	})), nil
}

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

// accessHint explains the usual cause of a 403: without s3:ListBucket, S3 answers 403 where it
// would answer 404.
func accessHint(err error) string {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusForbidden {
		return " (HTTP 403: the credentials need s3:GetObject, s3:PutObject, s3:DeleteObject and s3:ListBucket on the bucket)"
	}
	return ""
}

func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Put implements Bucket.
func (b *s3Bucket) Put(ctx context.Context, key string, src io.ReaderAt, size int64, meta FileMeta) error {
	if size > b.threshold {
		return b.putParts(ctx, key, src, size, meta)
	}
	_, err := b.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.bucket, Key: &key, Body: io.NewSectionReader(src, 0, size), ContentLength: aws.Int64(size),
		ContentType: str(meta.ContentType), CacheControl: str(meta.CacheControl),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w%s", key, err, accessHint(err))
	}
	return nil
}

func (b *s3Bucket) putParts(ctx context.Context, key string, src io.ReaderAt, size int64, meta FileMeta) (err error) {
	up, err := b.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &b.bucket, Key: &key, ContentType: str(meta.ContentType), CacheControl: str(meta.CacheControl),
	})
	if err != nil {
		return fmt.Errorf("s3 create multipart upload %s: %w%s", key, err, accessHint(err))
	}
	defer func() {
		if err != nil {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, _ = b.c.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: &b.bucket, Key: &key, UploadId: up.UploadId})
		}
	}()
	part := max(b.partSize, (size+maxParts-1)/maxParts)
	n := int((size + part - 1) / part)
	done := make([]types.CompletedPart, n)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(partsInFlight)
	for i := range n {
		g.Go(func() error {
			off := int64(i) * part
			length := min(part, size-off)
			out, err := b.c.UploadPart(gctx, &s3.UploadPartInput{
				Bucket: &b.bucket, Key: &key, UploadId: up.UploadId, PartNumber: aws.Int32(int32(i + 1)),
				Body: io.NewSectionReader(src, off, length), ContentLength: aws.Int64(length),
			})
			if err != nil {
				return fmt.Errorf("s3 upload part %d of %s: %w", i+1, key, err)
			}
			done[i] = types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1))}
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		return err
	}
	if _, err = b.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &b.bucket, Key: &key, UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done},
	}); err != nil {
		return fmt.Errorf("s3 complete multipart upload %s: %w", key, err)
	}
	return nil
}

// List implements Bucket.
func (b *s3Bucket) List(ctx context.Context, prefix string, fn func(Entry) error) error {
	pg := s3.NewListObjectsV2Paginator(b.c, &s3.ListObjectsV2Input{Bucket: &b.bucket, Prefix: &prefix})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3 list %s: %w%s", prefix, err, accessHint(err))
		}
		for _, o := range page.Contents {
			if err := fn(Entry{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Get implements Bucket.
func (b *s3Bucket) Get(ctx context.Context, key string) (io.ReadCloser, FileMeta, error) {
	out, err := b.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return nil, FileMeta{}, ErrNotFound
		}
		return nil, FileMeta{}, fmt.Errorf("s3 get %s: %w%s", key, err, accessHint(err))
	}
	return out.Body, FileMeta{ContentType: aws.ToString(out.ContentType), CacheControl: aws.ToString(out.CacheControl)}, nil
}

// Delete implements Bucket. It deletes one key per request: DeleteObjects wants a checksum
// header that some S3-compatible services do not accept, and a migration deletes little.
func (b *s3Bucket) Delete(ctx context.Context, keys ...string) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(deletesInFlight)
	for _, k := range keys {
		g.Go(func() error {
			if _, err := b.c.DeleteObject(gctx, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &k}); err != nil && !isNotFound(err) {
				return fmt.Errorf("s3 delete %s: %w%s", k, err, accessHint(err))
			}
			return nil
		})
	}
	return g.Wait()
}

// validKey says why path cannot be part of an S3 key, or returns "". Storage's S3 backend uses UTF-8
// keys of at most 1,024 bytes; a file name that is not valid UTF-8 has no key.
func validKey(key string) string {
	switch {
	case len(key) > 1024:
		return "the key would be longer than 1,024 bytes"
	case !utf8.ValidString(key):
		return "the name is not valid UTF-8"
	case strings.ContainsRune(key, 0):
		return "the name holds a NUL byte"
	}
	return ""
}
