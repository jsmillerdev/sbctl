package health

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// statfsDisk reads the free space available to unprivileged writers and the total size of
// the filesystem that holds path.
func statfsDisk(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

// checkDisk judges the free space on the state volume: under [health] disk_low_percent or
// disk_low_gb is a warning, under half of either is a failure that still only degrades.
func (d *Deps) checkDisk() Component {
	c := Component{Name: "disk", State: OK}
	read := d.Disk
	if read == nil {
		read = statfsDisk
	}
	free, total, err := read(d.Cfg.StateDir)
	if err != nil || total == 0 {
		c.State, c.Detail = Warn, "cannot read the free space of "+d.Cfg.StateDir
		return c
	}
	pct := float64(free) / float64(total) * 100
	lowPct, lowBytes := d.Cfg.Health.DiskLow()
	c.Detail = fmt.Sprintf("%.0f%% free (%s of %s) on %s", pct, humanBytes(free), humanBytes(total), d.Cfg.StateDir)
	switch {
	case pct < float64(lowPct)/2 || free < uint64(lowBytes)/2:
		c.State = Fail
	case pct < float64(lowPct) || free < uint64(lowBytes):
		c.State = Warn
	}
	return c
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// checkCertificates reads the certificates CertMagic keeps under <state_dir>/certs and reports
// the one that expires first. CertMagic renews 30 days ahead, so one inside
// [health] certificate_warn_days means renewal is failing.
func (d *Deps) checkCertificates() Component {
	c := Component{Name: "certificates", State: OK}
	if d.Cfg.TLS.Mode == "off" {
		c.Detail = "TLS is off"
		return c
	}
	name, end, n, err := earliestCertificate(d.Cfg.Paths().Certs())
	switch {
	case err != nil:
		c.State, c.Detail = Info, "cannot read the certificates: "+shorten(err.Error())
	case n == 0:
		c.State, c.Detail = Info, "none issued yet"
	default:
		left := end.Sub(d.now())
		switch {
		case left <= 0:
			c.State, c.Detail = Fail, fmt.Sprintf("the certificate for %s expired %s ago", name, humanAge(-left))
		case left < d.Cfg.Health.CertificateWarn():
			c.State, c.Detail = Warn, fmt.Sprintf("the certificate for %s expires in %s; renewal is not working", name, humanAge(left))
		default:
			c.Detail = fmt.Sprintf("%d issued, the first to expire is %s in %s", n, name, humanAge(left))
		}
	}
	return c
}

// earliestCertificate walks dir for PEM certificates (*.crt) and returns the name and expiry
// of the one that ends first, and how many it read. A directory that does not exist holds none.
func earliestCertificate(dir string) (name string, end time.Time, n int, err error) {
	walkErr := filepath.WalkDir(dir, func(path string, e fs.DirEntry, werr error) error {
		if werr != nil {
			if os.IsNotExist(werr) {
				return nil
			}
			return werr
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".crt") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // one unreadable file does not hide the others
		}
		block, _ := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil
		}
		cert, perr := x509.ParseCertificate(block.Bytes)
		if perr != nil {
			return nil
		}
		n++
		if end.IsZero() || cert.NotAfter.Before(end) {
			end = cert.NotAfter
			name = strings.TrimPrefix(e.Name()[:len(e.Name())-len(".crt")], "wildcard_")
			if len(cert.DNSNames) > 0 {
				name = cert.DNSNames[0]
			}
		}
		return nil
	})
	return name, end, n, walkErr
}
