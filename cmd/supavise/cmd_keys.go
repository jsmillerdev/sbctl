package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// The master key (/etc/supavise/master.key) unseals the passwords inside every backup and
// is in none of them in the clear. These commands put it where a lost server cannot take it
// along: offline with the operator (export-key), or encrypted with an operator passphrase in
// the backup backend (escrow-key, restore-key).

func init() {
	var keyOnly bool
	export := &cobra.Command{
		Use:   "export-key",
		Short: "Print the master key and the configuration for safekeeping offline",
		Long: "Prints the node's master key (key_path) and config.toml. Store the output somewhere that\n" +
			"survives losing this server and is not the backup bucket: a password manager, or a printed\n" +
			"copy in a safe. The key unseals the database passwords inside every backup; with it and a\n" +
			"backup, a new node can be rebuilt, and with it alone an attacker who reaches the backups\n" +
			"can read those passwords. config.toml may hold S3 and DNS credentials.\n\n" +
			"  --key-only   print just the key, for a pipe into a password manager",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			key, err := readMasterKey(cfg.KeyPath)
			if err != nil {
				return err
			}
			if keyOnly {
				fmt.Fprintln(cmd.OutOrStdout(), key)
				return nil
			}
			cfgText, cfgPath := readConfigText()
			fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: this output is the key to the passwords in your backups. Store it offline and keep it out of the backup bucket, chat and email.")
			printKeyExport(cmd.OutOrStdout(), key, cfgPath, cfgText, time.Now())
			return nil
		},
	}
	export.Flags().BoolVar(&keyOnly, "key-only", false, "print only the key")

	var escrowPass string
	escrow := &cobra.Command{
		Use:   "escrow-key --passphrase-file <file>",
		Short: "Keep an encrypted copy of the master key and config.toml in the backup backend",
		Long: "Encrypts the master key and config.toml with a passphrase only you know (argon2id and\n" +
			"AES-256-GCM) and stores the result in the backup backend, next to the backups. Rebuild a\n" +
			"lost server with `supavise system restore-key`. The key itself never leaves this server in\n" +
			"the clear, and nothing here stores the passphrase: write it down offline. Without it the\n" +
			"copy cannot be opened. Run the command again after you change config.toml.\n\n" +
			"The passphrase file holds the passphrase (at least " + fmt.Sprint(backup.MinPassphraseLen) + " characters) and must be mode 0600; use - to read it from\n" +
			"standard input.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			pass, err := readPassphrase(escrowPass, cmd.InOrStdin())
			if err != nil {
				return err
			}
			key, err := readMasterKey(cfg.KeyPath)
			if err != nil {
				return err
			}
			cfgText, _ := readConfigText()
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			st, err := backup.OpenStore(ctx, cfg.Backup)
			if err != nil {
				return err
			}
			if err := backup.PutKeyEscrow(ctx, st, pass, backup.EscrowContents{MasterKey: key, ConfigTOML: cfgText}, time.Now()); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "encrypted copy of the master key (key id %s) and config.toml stored at %s\n", backup.KeyID(key), st.URL(backup.EscrowKey))
			fmt.Fprintln(w, "Keep the passphrase offline: nothing on this server stores it, and without it the copy cannot be opened.")
			if strings.HasPrefix(cfg.Backup.Backend, "file://") {
				fmt.Fprintln(w, "WARNING: the backend is a directory on this server, so this copy is lost with the server's disk. Copy it elsewhere, or use an S3 bucket.")
			}
			return nil
		},
	}
	escrow.Flags().StringVar(&escrowPass, "passphrase-file", "", "file holding the passphrase (mode 0600), or - for standard input")
	_ = escrow.MarkFlagRequired("passphrase-file")

	var rPass, rKeyOut, rConfigOut string
	var rForce bool
	restoreKey := &cobra.Command{
		Use:   "restore-key --passphrase-file <file>",
		Short: "Write the master key back from its encrypted copy in the backup backend",
		Long: "For a node that lost its master key: reads the encrypted copy that `escrow-key` stored,\n" +
			"opens it with the passphrase and writes the key to key_path (or --key-out), mode 0600. The\n" +
			"command needs a config.toml that names the backend and its credentials; with --config-out\n" +
			"it also writes the config.toml saved in the copy. It never replaces a different key unless\n" +
			"--force is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			pass, err := readPassphrase(rPass, cmd.InOrStdin())
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			st, err := backup.OpenStore(ctx, cfg.Backup)
			if err != nil {
				return err
			}
			c, err := backup.GetKeyEscrow(ctx, st, pass)
			if errors.Is(err, backup.ErrNotFound) {
				return fmt.Errorf("the backend %s holds no key escrow (it is made by `supavise system escrow-key`)", cfg.Backup.Backend)
			}
			if err != nil {
				return err
			}
			if _, err := secrets.Load([]byte(c.MasterKey)); err != nil {
				return fmt.Errorf("the escrow does not hold a usable key: %w", err)
			}
			out := rKeyOut
			if out == "" {
				out = cfg.KeyPath
			}
			w := cmd.OutOrStdout()
			switch cur, rerr := os.ReadFile(out); {
			case rerr == nil && strings.TrimSpace(string(cur)) == c.MasterKey:
				fmt.Fprintf(w, "%s already holds this key (key id %s)\n", out, backup.KeyID(c.MasterKey))
			case rerr == nil && !rForce:
				return fmt.Errorf("%s holds a different key (key id %s); the escrow's is %s. Nothing was changed: pass --force to replace it", out, backup.KeyID(string(cur)), backup.KeyID(c.MasterKey))
			case rerr != nil && !errors.Is(rerr, fs.ErrNotExist):
				return rerr
			default:
				if err := writeSecretFileAtomic(out, []byte(c.MasterKey+"\n")); err != nil {
					return err
				}
				fmt.Fprintf(w, "master key (key id %s) written to %s\n", backup.KeyID(c.MasterKey), out)
			}
			if rConfigOut != "" {
				if c.ConfigTOML == "" {
					fmt.Fprintln(w, "the escrow holds no config.toml")
				} else if _, serr := os.Stat(rConfigOut); serr == nil && !rForce {
					return fmt.Errorf("%s exists: pass --force to replace it", rConfigOut)
				} else if err := writeSecretFileAtomic(rConfigOut, []byte(c.ConfigTOML)); err != nil {
					return err
				} else {
					fmt.Fprintf(w, "config.toml written to %s\n", rConfigOut)
				}
			}
			fmt.Fprintf(w, "Make the files belong to the supavise user: chown supavise:supavise %s\n", out)
			return nil
		},
	}
	restoreKey.Flags().StringVar(&rPass, "passphrase-file", "", "file holding the passphrase (mode 0600), or - for standard input")
	restoreKey.Flags().StringVar(&rKeyOut, "key-out", "", "where to write the key (default key_path)")
	restoreKey.Flags().StringVar(&rConfigOut, "config-out", "", "also write the config.toml saved in the escrow to this path")
	restoreKey.Flags().BoolVar(&rForce, "force", false, "replace a different key or an existing config file")
	_ = restoreKey.MarkFlagRequired("passphrase-file")

	systemCmd.AddCommand(export, escrow, restoreKey)
}

// readMasterKey reads and checks the hex key at path.
func readMasterKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the master key at %s (run as the supavise user or root): %w", path, err)
	}
	if _, err := secrets.Load(b); err != nil {
		return "", fmt.Errorf("%s is not a master key: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readConfigText returns the text and path of the config file in use; the text is empty
// when the node is configured by environment variables only.
func readConfigText() (text, path string) {
	path = configFilePath()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", path
	}
	return string(b), path
}

func printKeyExport(w io.Writer, key, cfgPath, cfgText string, now time.Time) {
	fmt.Fprintf(w, "# Supavise master key and configuration, exported %s.\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "# Keep this offline (a password manager, or a printed copy in a safe). Do not put it in the backup bucket.")
	fmt.Fprintln(w, "# The key unseals the database passwords inside every backup. config.toml may hold S3 and DNS credentials.")
	fmt.Fprintf(w, "master_key = %s\n", key)
	fmt.Fprintf(w, "key_id = %s\n", backup.KeyID(key))
	fmt.Fprintf(w, "\n# %s\n", cfgPath)
	if cfgText == "" {
		fmt.Fprintln(w, "# (no config file: the node is configured by SUPAVISE_* environment variables)")
		return
	}
	fmt.Fprint(w, cfgText)
	if !strings.HasSuffix(cfgText, "\n") {
		fmt.Fprintln(w)
	}
}

// readPassphrase reads the passphrase from the file at path (or stdin for "-"), strips one
// trailing newline and refuses a short one. A file other users can read is refused: a
// passphrase there protects nothing.
func readPassphrase(path string, stdin io.Reader) ([]byte, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(io.LimitReader(stdin, 4096))
	} else {
		var fi fs.FileInfo
		if fi, err = os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s is mode %04o; the passphrase file must be readable by its owner only (chmod 600 %s)", path, fi.Mode().Perm(), path)
		}
		if err == nil {
			b, err = os.ReadFile(path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("passphrase file: %w", err)
	}
	b = bytes.TrimSuffix(bytes.TrimSuffix(b, []byte("\n")), []byte("\r"))
	if err := backup.CheckPassphrase(b); err != nil {
		return nil, err
	}
	return b, nil
}

// writeSecretFileAtomic writes b to path with mode 0600 through a temporary file in the same
// directory, so a crash leaves the old file or the whole new one.
func writeSecretFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
