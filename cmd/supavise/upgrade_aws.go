package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/nodeupgrade"
)

// The stack layer of `supavise upgrade`: with --aws the release's signed script and template are
// staged and `supavise-aws-deploy.sh update` runs with the operator's own AWS credentials. The
// node's instance role never changes its stack (internal/nodeupgrade, "The AWS stack").

var (
	stackNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)
	setRe       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*=[^\s]*$`)
)

// checkAWSFlags refuses the stack flags without --aws, and a stack name or a parameter that is not
// one, before the node is read.
func checkAWSFlags(aws bool, stackName string, sets []string) error {
	if !aws {
		if stackName != "" || len(sets) > 0 {
			return errors.New("--stack-name and --set belong to --aws")
		}
		return nil
	}
	if stackName != "" && !stackNameRe.MatchString(stackName) {
		return fmt.Errorf("--stack-name %q is not a CloudFormation stack name", stackName)
	}
	for _, s := range sets {
		if !setRe.MatchString(s) {
			return fmt.Errorf("--set %q: want Parameter=Value, for example Failover=on", s)
		}
	}
	return nil
}

// dmiDir is where the firmware describes the machine.
var dmiDir = "/sys/devices/virtual/dmi/id"

// ec2Likely says whether this machine looks like an EC2 instance, without a network call: its
// firmware names Amazon, or a metadata endpoint is configured (a test). The cloud questions are
// asked only then, because a machine that is not on EC2 waits several seconds for each.
func ec2Likely() bool {
	if os.Getenv(awsapi.EnvEndpointIMDS) != "" {
		return true
	}
	for _, f := range []string{"sys_vendor", "board_vendor", "bios_vendor"} {
		if b, err := os.ReadFile(filepath.Join(dmiDir, f)); err == nil && strings.Contains(strings.ToLower(string(b)), "amazon") {
			return true
		}
	}
	return false
}

// awsCredentialsPresent says whether the environment holds AWS credentials of the operator's: the
// standard variables, a web identity or container credential source, a shared credentials file, or
// a config file that names where credentials come from. The instance role does not count; the
// update script refuses to use it. A config file with a region and nothing else is not a credential
// (it is what an instance's own account often has), and neither is one the operator did not select
// a profile for.
func awsCredentialsPresent(getenv func(string) string, home string) bool {
	if getenv("AWS_ACCESS_KEY_ID") != "" && getenv("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}
	for _, k := range []string{"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		if getenv(k) != "" {
			return true
		}
	}
	credFiles := []string{getenv("AWS_SHARED_CREDENTIALS_FILE")}
	cfgFiles := []string{getenv("AWS_CONFIG_FILE")}
	if home != "" {
		credFiles = append(credFiles, filepath.Join(home, ".aws", "credentials"))
		cfgFiles = append(cfgFiles, filepath.Join(home, ".aws", "config"))
	}
	for _, f := range credFiles {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	for _, f := range cfgFiles {
		if f != "" && (getenv("AWS_PROFILE") != "" || awsConfigNamesCredentials(f)) {
			if _, err := os.Stat(f); err == nil {
				return true
			}
		}
	}
	return false
}

// awsConfigKeys are the settings of an AWS config file that say where credentials come from: keys
// of their own, an SSO session, an assumed role, a helper program or a web identity.
var awsConfigKeys = []string{"aws_access_key_id", "sso_session", "sso_start_url", "sso_account_id", "role_arn", "credential_process", "credential_source", "source_profile", "web_identity_token_file"}

// awsConfigNamesCredentials reports whether the AWS config file at path has a setting of
// awsConfigKeys.
func awsConfigNamesCredentials(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		for _, k := range awsConfigKeys {
			if key == k {
				return true
			}
		}
	}
	return false
}

// awsUpdateArgs are the arguments of `supavise-aws-deploy.sh` for the stack update. The parameters
// of the stack are the ones it has (UsePreviousValue), so a parameter the operator does not name
// keeps its value.
func awsUpdateArgs(stack, template string, sets []string) []string {
	args := []string{"update", "--stack", stack, "--template", template, "--params-from-stack"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	return args
}

// awsCommand is the command that runs the same update by hand, for a node that found no
// credentials.
func awsCommand(stack string, sets []string) string {
	cmd := "sudo -E supavise upgrade --aws --stack-name " + shellQuote(stack)
	for _, s := range sets {
		cmd += " --set " + shellQuote(s)
	}
	return cmd
}

// awsStandalone is the same update as the signed script runs it on its own: for an operator whose
// credentials live in CloudShell or on a laptop and not on the node. The script reads the region
// from AWS_REGION or --region, so the node's region is named when it is known.
func awsStandalone(stack, region string, sets []string) string {
	if region == "" {
		region = "REGION"
	}
	cmd := "supavise-aws-deploy.sh update --stack " + shellQuote(stack) + " --region " + shellQuote(region)
	for _, s := range sets {
		cmd += " --set " + shellQuote(s)
	}
	return cmd
}

// shellQuote quotes s for display, only when a shell would need it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./:=@%+,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stackName finds the CloudFormation stack that made this server: the flag, else [aws] stack_name
// (config.d/20-aws.toml), else the instance's supavise:stack-name tag. A stack from before the tags
// has to be named once, and the update then records it.
func (h *nodeHost) stackName(ctx context.Context, given string) (string, error) {
	name := given
	if name == "" {
		name = h.cfg.AWS.StackName
	}
	if name == "" {
		// Reading what the instance is does not use its role. An operator who set
		// AWS_EC2_METADATA_DISABLED so that no tool takes the role's credentials (the update script
		// sets it too) still gets the instance's tags read here, before the script runs.
		c, err := awsapi.New(awsapi.Config{NoInstanceRole: true, Getenv: hideMetadataSwitch})
		if err == nil {
			if tags, err := c.IMDS.Tags(ctx); err == nil {
				name = tags["supavise:stack-name"]
			}
		}
	}
	if name == "" {
		return "", errors.New("cannot tell which CloudFormation stack made this server (the instance has no supavise:stack-name tag and [aws] stack_name is not set): name it with --stack-name")
	}
	if !stackNameRe.MatchString(name) {
		return "", fmt.Errorf("%q is not a CloudFormation stack name", name)
	}
	return name, nil
}

// UpdateStack implements nodeupgrade.StackUpdater.
func (h *nodeHost) UpdateStack(ctx context.Context, c *nodeupgrade.Candidate, so nodeupgrade.StackOptions) (nodeupgrade.StackOutcome, error) {
	stack, err := h.stackName(ctx, so.Name)
	if err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	if err := checkAWSFlags(true, "", so.Sets); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	home, _ := os.UserHomeDir()
	if !awsCredentialsPresent(os.Getenv, home) {
		return nodeupgrade.StackOutcome{Command: awsCommand(stack, so.Sets), Standalone: awsStandalone(stack, h.region(ctx), so.Sets)}, nil
	}
	r, ok := c.Data.(*resolved)
	if !ok {
		return nodeupgrade.StackOutcome{}, errors.New("internal error: the release was not resolved")
	}
	assets, err := r.ver.AWSAssets(ctx, r.opts)
	if err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	// A private directory of root's: the script is run from here and the template is read from here,
	// and neither may be changed between the check and the run.
	dir, err := os.MkdirTemp("", "supavise-aws-")
	if err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	defer os.RemoveAll(dir)
	script, template := filepath.Join(dir, "supavise-aws-deploy.sh"), filepath.Join(dir, assets.TemplateName)
	if err := os.WriteFile(script, assets.Script, 0o700); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	if err := os.WriteFile(template, assets.Template, 0o600); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}

	// The script asks the operator to type "apply" after it shows the change set, so it keeps the
	// terminal. It is run by bash, not executed, so a noexec /tmp does not matter. The script and the
	// template are the bytes that AWSAssets checked against the signed list, so nothing is left for the
	// script's own check of itself, which only runs on its download path.
	//
	// It is not tied to ctx: a cancelled upgrade (Ctrl-C, SIGTERM) must not kill it, because while an
	// update runs under its guard (a temporary stack policy) the script waits for the update to end and
	// then takes the guard off. Killed, it would leave the guard on and the update running unseen.
	// runStackScript passes the signals on instead, and the script decides.
	if err := ctx.Err(); err != nil {
		return nodeupgrade.StackOutcome{}, err
	}
	cmd := exec.Command("bash", append([]string{script}, awsUpdateArgs(stack, template, so.Sets)...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, h.out, h.errw
	// The script takes its trust root (the release key, the download address) and the address of the
	// metadata service it checks the node's identity against from these variables when they are set,
	// for tests. `sudo -E` passes on whatever the caller exported, and the stack is changed with this
	// script: the variables do not reach it.
	cmd.Env = withoutEnv(os.Environ(), scriptTestHookVars...)
	// The script exits 0 (updated, or nothing to change), 2 (refused: nothing was changed), 3 (failed,
	// also a guarded update that CloudFormation rolled back after the temporary stack policy denied a
	// replacement), 4 (updated, but its temporary stack policy could not be taken off), or 129, 130 or
	// 143 after an interrupt (HUP, INT, TERM). Its review's own statuses (10 refused, 11 blocked, 12
	// guarded) stay inside it.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var out nodeupgrade.StackOutcome
	if err := runStackScript(cmd, sigs); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return out, fmt.Errorf("running supavise-aws-deploy.sh: %w", err)
		}
		code := ee.ExitCode()
		if code != 4 {
			return out, &nodeupgrade.StackError{Code: code, Err: fmt.Errorf("supavise-aws-deploy.sh update exited with status %d", code)}
		}
		out.GuardLeftOn = true
	}
	// The stack is known now: later runs and `supavise status` need not be told its name.
	if err := writeAWSConfig(filepath.Dir(h.cfgPath), stack); err != nil {
		fmt.Fprintf(h.errw, "warning: could not record the stack name in config.d: %v\n", err)
	}
	return out, nil
}

// runStackScript runs the stack script and waits for it, passing on each signal from sigs. A Ctrl-C
// at the terminal reaches the script by itself as well; it takes one that comes twice within two
// seconds for one. A signal the script has no trap for (before its guard is set) ends it, as it would
// end it at the terminal.
func runStackScript(cmd *exec.Cmd, sigs <-chan os.Signal) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
		}
	}
}

// scriptTestHookVars are the variables that replace what supavise-aws-deploy.sh trusts: the release
// key, the address the release is fetched from and the address of the metadata service.
var scriptTestHookVars = []string{"SUPAVISE_DEPLOY_PUBKEY_B64", "SUPAVISE_DEPLOY_BASE_URL", "SUPAVISE_IMDS_ENDPOINT"}

// withoutEnv returns env without the variables named.
func withoutEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// region is the region the instance runs in, read from the metadata service without its role
// credentials, or "" when it cannot be read.
func (h *nodeHost) region(ctx context.Context) string {
	c, err := awsapi.New(awsapi.Config{NoInstanceRole: true, Getenv: hideMetadataSwitch})
	if err != nil {
		return ""
	}
	r, err := c.IMDS.Region(ctx)
	if err != nil {
		return ""
	}
	return r
}

// hideMetadataSwitch is os.Getenv without AWS_EC2_METADATA_DISABLED. An operator sets that switch so
// that no tool takes the instance role's credentials, and the update script sets it itself; reading
// the instance's identity and tags takes no credentials, so these reads ignore it.
func hideMetadataSwitch(k string) string {
	if k == "AWS_EC2_METADATA_DISABLED" {
		return ""
	}
	return os.Getenv(k)
}

// writeAWSConfig records the node's stack in config.d/20-aws.toml. The file is read by the
// supavise user, so it is not root's alone; it holds no secret.
func writeAWSConfig(configDir, stack string) error {
	// The name comes from an instance tag when first boot records it; a name that is not a stack name
	// could make the file invalid TOML, and the daemon would stop loading its configuration.
	if !stackNameRe.MatchString(stack) {
		return fmt.Errorf("%q is not a CloudFormation stack name", stack)
	}
	path := filepath.Join(configDir, config.ConfigDName, config.AWSConfigFile)
	body := fmt.Sprintf("# Written by `supavise upgrade --aws`: the CloudFormation stack that made this server.\n[aws]\nstack_name = %q\n", stack)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return nil
	}
	uid, gid := supaviseOwner()
	return writeFileAtomic(path, []byte(body), 0o644, uid, gid)
}
