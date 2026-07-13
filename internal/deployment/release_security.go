package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const releaseWorkflowIdentity = "https://github.com/BarqDB/barq-control-plane/.github/workflows/release.yml@refs/tags/"
const githubActionsIssuer = "https://token.actions.githubusercontent.com"

func VerifyRelease(ctx context.Context, release Release, runner Runner, stdout, stderr io.Writer) error {
	if release.Version == "" || strings.ContainsAny(release.Version, "/\\?# ") {
		return errors.New("release version cannot be verified")
	}
	if !fixedImage(release.ControlImage) || !fixedImage(release.CoreImage) {
		return errors.New("only fixed image digests can be verified")
	}
	runner = defaultRunner(runner)
	stdout, stderr = defaultWriter(stdout), defaultWriter(stderr)
	identity := releaseWorkflowIdentity + release.Version
	for _, image := range []string{release.CoreImage, release.ControlImage} {
		if err := runner.Run(ctx, "", nil, stdout, stderr, nil, cosignExecutable(),
			"verify", "--certificate-identity", identity, "--certificate-oidc-issuer", githubActionsIssuer, image); err != nil {
			return fmt.Errorf("verify signed image %s: %w", image, err)
		}
	}
	return nil
}

func cosignExecutable() string {
	executable, err := os.Executable()
	if err == nil {
		name := "cosign"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		bundled := filepath.Join(filepath.Dir(executable), name)
		if info, err := os.Stat(bundled); err == nil && !info.IsDir() {
			return bundled
		}
	}
	return "cosign"
}
