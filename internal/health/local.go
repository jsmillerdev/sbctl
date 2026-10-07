package health

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
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

// checkCertificates reads the certificates CertMagic keeps under <state_dir>/certs and judges
// the ones the node must keep valid: api.<domain>, studio.<domain> and the wildcard, which
// CertMagic renews in the background 30 days ahead, so one inside [health] certificate_warn_days
// means renewal is failing.
//
// A certificate for a project host or a custom hostname is issued on demand and renewed only
// when someone opens that host, so it can sit near its end without anything being wrong, and one
// for a host the node no longer serves (a deleted project, a removed hostname) is never renewed
// at all. Those do not judge the node: the report only counts the near ones as notes.
func (d *Deps) checkCertificates(ctx context.Context) Component {
	c := Component{Name: "certificates", State: OK}
	if d.Cfg.TLS.Mode == "off" {
		c.Detail = "TLS is off"
		return c
	}
	certs, err := readCertificates(d.Cfg.Paths().Certs())
	if err != nil {
		c.State, c.Detail = Info, "cannot read the certificates: "+shorten(err.Error())
		return c
	}
	managed := d.managedCertNames()
	live := d.liveHosts(ctx)
	warn := d.Cfg.Health.CertificateWarn()
	now := d.now()
	var (
		first              *certInfo
		nearLive, nRetired int
		// newest is, for each managed name, the certificate that ends last. CertMagic keeps one
		// copy per issuer, so a name can have an old copy that is never renewed (after [tls] ca or
		// the issuer changed); only the newest says whether the name is covered.
		newest = map[string]int{}
	)
	for i, ci := range certs {
		switch {
		case ci.anyOf(managed):
			for _, n := range ci.names {
				if j, ok := newest[n]; managed[n] && (!ok || ci.end.After(certs[j].end)) {
					newest[n] = i
				}
			}
		case ci.anyOf(live):
			if ci.end.Sub(now) < warn {
				nearLive++
			}
		default:
			nRetired++
		}
	}
	covering := map[int]bool{}
	for _, i := range newest {
		covering[i] = true
		if first == nil || certs[i].end.Before(first.end) {
			first = &certs[i]
		}
	}
	nManaged := len(covering)
	switch {
	case first == nil:
		c.State, c.Detail = Info, "none issued yet for "+d.Cfg.APIHost()+" and "+d.Cfg.StudioHost()
	default:
		left := first.end.Sub(now)
		switch {
		case left <= 0:
			c.State, c.Detail = Fail, fmt.Sprintf("the certificate for %s expired %s ago", first.name(), humanAge(-left))
		case left < warn:
			c.State, c.Detail = Warn, fmt.Sprintf("the certificate for %s expires in %s; renewal is not working", first.name(), humanAge(left))
		default:
			c.Detail = fmt.Sprintf("%d issued, the first to expire is %s in %s", nManaged, first.name(), humanAge(left))
		}
	}
	if nearLive > 0 {
		c.Detail += fmt.Sprintf("; %d project host certificate(s) are near their end and renew at the next visit", nearLive)
	}
	if nRetired > 0 {
		c.Detail += fmt.Sprintf("; %d certificate(s) for hosts this node no longer serves are ignored", nRetired)
	}
	return c
}

// managedCertNames are the names of the certificates the proxy keeps valid from startup.
func (d *Deps) managedCertNames() map[string]bool {
	return map[string]bool{
		d.Cfg.APIHost(): true, d.Cfg.StudioHost(): true, "*.api." + d.Cfg.BaseDomain(): true,
	}
}

// liveHosts are the project hosts and custom hostnames the node serves now. Without a registry
// it is empty, and every on-demand certificate counts as retired.
func (d *Deps) liveHosts(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if d.Registry == nil {
		return out
	}
	ps, err := d.Registry.ListProjects(ctx)
	if err != nil {
		return out
	}
	liveRef := map[string]bool{}
	for _, p := range ps {
		if p.Ref == config.SystemRef || p.Status == registry.StatusRemoved || p.Status == registry.StatusInitFailed {
			continue
		}
		liveRef[p.Ref] = true
		out[strings.ToLower(d.Cfg.ProjectHost(p.Ref))] = true
	}
	if rs, err := d.Registry.ListRoutes(ctx); err == nil {
		for _, r := range rs {
			if liveRef[r.Ref] {
				out[strings.ToLower(r.Host)] = true
			}
		}
	}
	return out
}

// certInfo is one certificate CertMagic stored.
type certInfo struct {
	names []string
	end   time.Time
}

func (ci certInfo) name() string { return ci.names[0] }

func (ci certInfo) anyOf(set map[string]bool) bool {
	for _, n := range ci.names {
		if set[n] {
			return true
		}
	}
	return false
}

// readCertificates walks dir for PEM certificates (*.crt). A directory that does not exist
// holds none, and a file that is not a certificate is skipped.
func readCertificates(dir string) ([]certInfo, error) {
	var out []certInfo
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, werr error) error {
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
		ci := certInfo{end: cert.NotAfter}
		for _, n := range cert.DNSNames {
			ci.names = append(ci.names, strings.ToLower(n))
		}
		if len(ci.names) == 0 {
			// CertMagic names the file after the certificate, a wildcard as "wildcard_.example.com".
			n := strings.TrimSuffix(e.Name(), ".crt")
			ci.names = []string{strings.ToLower(strings.Replace(n, "wildcard_", "*", 1))}
		}
		out = append(out, ci)
		return nil
	})
	return out, err
}
