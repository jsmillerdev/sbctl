package cloudformation_test

// The user data of a replica server run for real: the script of the template with its variables
// filled in, in a temporary directory, with stand-ins for curl, apt-get and the AWS CLI. What it
// shows is the order of trust: the installer runs only when the signature of the release's checksum
// list verifies against the key in the template and the installer is the file the list names, and the
// join token is read after that, into a file only the owner reads.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const stubCurl = `#!/bin/bash
echo "curl $*" >> "$STUB_DIR/curl.log"
out="" url="" data=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    --data-binary) data=$2; shift 2 ;;
    -X | -H | -m | --retry) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "$url" in
  *checkip.amazonaws.com) if [ -n "$out" ] && [ "$out" != /dev/null ]; then echo 203.0.113.9 > "$out"; elif [ -z "$out" ]; then echo 203.0.113.9; fi ;;
  file://*) cp "${url#file://}" "$out" ;;
  https://handle.example/signal) echo "$data" >> "$STUB_DIR/signals.log" ;;
esac
`

const stubInstaller = `#!/bin/bash
echo "$*" > "$STUB_DIR/install.args"
prev=""
for a in "$@"; do
  if [ "$prev" = --join-token-file ]; then
    cat "$a" > "$STUB_DIR/install.token"
    (stat -c %a "$a" 2>/dev/null || stat -f %Lp "$a") > "$STUB_DIR/install.tokenmode"
  fi
  prev=$a
done
exit "${INSTALL_RC-0}"
`

type stubRun struct {
	signals, args, token, mode, out string
	installed                       bool
	tokenRead                       bool
	rc                              int
}

// runReplicaUserData runs the replica user data. tamper edits the release directory before the run;
// keyB64 is the key put in the template (the marker itself when empty).
func runReplicaUserData(t *testing.T, tamper func(rel string, pub ed25519.PublicKey, priv ed25519.PrivateKey), noKey bool) stubRun {
	t.Helper()
	ssl := findOpenSSL(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	tmp := t.TempDir()
	root, bin, rel := filepath.Join(tmp, "root"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "rel")
	for _, d := range []string{root, bin, rel} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyB64 := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	inst := []byte(stubInstaller)
	h := sha256.Sum256(inst)
	sums := fmt.Sprintf("%s  install.sh\n", hex.EncodeToString(h[:]))
	for name, b := range map[string][]byte{"install.sh": inst, "SHA256SUMS": []byte(sums), "SHA256SUMS.sig": ed25519.Sign(priv, []byte(sums))} {
		if err := os.WriteFile(filepath.Join(rel, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if tamper != nil {
		tamper(rel, pub, priv)
	}

	script, _ := userDataSub(t, true)
	values := map[string]string{
		"${InstallHandle}": "https://handle.example/signal", "${AWS::Region}": "eu-west-1", "${ElasticIp}": "203.0.113.9",
		"${ReleaseBase}": "file://" + rel, "${JoinTokenPolicy}": "policy", "${AdminEmail}": "owner@example.com",
		"${JoinTokenSecretArn}": "arn:aws:secretsmanager:eu-west-1:111122223333:secret:join-AbCdEf",
		"${BackupBucketName}":   "leader-backup", "${BackupBucketRegion}": "us-east-1", "${VersionArgs}": "true",
	}
	script = regexp.MustCompile(`\$\{[^}]+\}`).ReplaceAllStringFunc(script, func(m string) string {
		v, ok := values[m]
		if !ok {
			t.Fatalf("the replica user data uses %s, which this test does not know how to fill in", m)
		}
		return v
	})
	if !noKey {
		script = strings.ReplaceAll(script, keyMarker, keyB64)
	}
	script = strings.ReplaceAll(script, "/root/", root+"/")
	script = strings.ReplaceAll(script, "/var/log/supavise-bootstrap.log", filepath.Join(tmp, "bootstrap.log"))

	stubs := map[string]string{
		"curl": stubCurl, "apt-get": "#!/bin/sh\nexit 0\n", "snap": "#!/bin/sh\nexit 0\n", "sleep": "#!/bin/sh\nexit 0\n",
		"aws": "#!/bin/sh\ncase \"$*\" in *get-secret-value*) printf 'svj1.TESTTOKEN' ;; esac\n",
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		stubs["sha256sum"] = "#!/bin/sh\nexec shasum -a 256 \"$@\"\n"
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":" + filepath.Dir(ssl) + ":/usr/bin:/bin", "STUB_DIR=" + tmp, "HOME=" + tmp}
	out, err := cmd.CombinedOutput()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(tmp, name))
		return strings.TrimSpace(string(b))
	}
	_, tokErr := os.Stat(filepath.Join(root, "join-token"))
	return stubRun{signals: read("signals.log"), args: read("install.args"), token: read("install.token"), mode: read("install.tokenmode"),
		out: string(out), installed: read("install.args") != "", tokenRead: tokErr == nil || read("install.token") != "", rc: rc}
}

func TestReplicaUserDataRunsTheVerifiedInstaller(t *testing.T) {
	r := runReplicaUserData(t, nil, false)
	if r.rc != 0 || !strings.Contains(r.signals, `"Status":"SUCCESS"`) {
		t.Fatalf("exit %d, signals %q\n%s", r.rc, r.signals, r.out)
	}
	root := r.args
	for _, want := range []string{"--email owner@example.com", "--s3-bucket leader-backup", "--s3-region us-east-1", "--public-ip 203.0.113.9",
		"--aws-first-boot", "/join-token", "--firewall none", "--region eu-west-1"} {
		if !strings.Contains(root, want) {
			t.Errorf("the installer was run with %q, lacks %q", root, want)
		}
	}
	// The token reached the installer through the file, and the file is private.
	if r.token != "svj1.TESTTOKEN" || r.mode != "600" {
		t.Errorf("token file: content %q mode %q", r.token, r.mode)
	}
	if strings.Contains(r.args, "TESTTOKEN") || strings.Contains(r.out, "TESTTOKEN") {
		t.Error("the token is on the installer's command line or in the log")
	}
}

func TestReplicaUserDataRefusesAnInstallerThatIsNotTheSignedOne(t *testing.T) {
	for name, c := range map[string]struct {
		tamper func(rel string, pub ed25519.PublicKey, priv ed25519.PrivateKey)
		noKey  bool
		want   string
	}{
		"installer edited": {func(rel string, _ ed25519.PublicKey, _ ed25519.PrivateKey) {
			os.WriteFile(filepath.Join(rel, "install.sh"), []byte("#!/bin/bash\ntouch \"$STUB_DIR/pwned\"\n"), 0o755)
		}, false, "install.sh does not match the signed checksum list"},
		"list edited": {func(rel string, _ ed25519.PublicKey, _ ed25519.PrivateKey) {
			os.WriteFile(filepath.Join(rel, "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  install.sh\n"), 0o644)
		}, false, "signature of SHA256SUMS does not verify"},
		"signed by another key": {func(rel string, _ ed25519.PublicKey, _ ed25519.PrivateKey) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			sums, _ := os.ReadFile(filepath.Join(rel, "SHA256SUMS"))
			os.WriteFile(filepath.Join(rel, "SHA256SUMS.sig"), ed25519.Sign(other, sums), 0o644)
		}, false, "signature of SHA256SUMS does not verify"},
		"installer missing from the list": {func(rel string, _ ed25519.PublicKey, priv ed25519.PrivateKey) {
			sums := []byte(strings.Repeat("0", 64) + "  other.sh\n")
			os.WriteFile(filepath.Join(rel, "SHA256SUMS"), sums, 0o644)
			os.WriteFile(filepath.Join(rel, "SHA256SUMS.sig"), ed25519.Sign(priv, sums), 0o644)
		}, false, "install.sh does not match the signed checksum list"},
		"template without a release key": {nil, true, "this template holds no release key"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runReplicaUserData(t, c.tamper, c.noKey)
			if r.rc == 0 || !strings.Contains(r.signals, `"Status":"FAILURE"`) || !strings.Contains(r.signals, c.want) {
				t.Errorf("exit %d, signals %q, want a FAILURE that says %q\n%s", r.rc, r.signals, c.want, r.out)
			}
			if r.installed {
				t.Error("the installer ran although it was not the signed one")
			}
			if r.tokenRead {
				t.Error("the join token was read before the installer was verified")
			}
		})
	}
}
