package pirn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Limits on the files an agent step keeps in a registry.
const (
	// MaxLargeFile is the largest file kept in a registry.
	MaxLargeFile = 4 << 30
	// MaxLargeBytes is how much one step keeps in a registry in all.
	MaxLargeBytes = 16 << 30
)

// FileType is the OCI artifact type of a file an agent step kept in a
// registry.
const FileType = "application/vnd.heedle.file"

// Registry keeps the files an agent step leaves in /work/out that are too
// big for its record, in an OCI registry, with the oras CLI. oras uses the
// worker's own registry login (oras login, or Docker's credentials), so
// the step never sees it. Each file is one artifact: its one layer is the
// file, so its digest is the file's own SHA-256, and its tag is
// "sha256-<hex>" so registries that collect untagged manifests keep it.
type Registry struct {
	Oras      string // the oras binary; empty means "oras" on PATH
	Repo      string // the repository, such as ghcr.io/acme/heedle-files
	PlainHTTP bool   // for a registry without TLS, on localhost in tests
}

// repoRef is a repository with no tag or digest: a registry host (with an
// optional port) and a lower-case path, as OCI's distribution spec names
// them.
var repoRef = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`)

// check reports whether r can be used.
func (r *Registry) check() error {
	if !repoRef.MatchString(r.Repo) {
		return fmt.Errorf("%w: registry repository %q (want host/path, lower case, without a tag or digest)", ErrSetup, r.Repo)
	}
	return nil
}

func (r *Registry) oras() string {
	if r.Oras != "" {
		return r.Oras
	}
	return "oras"
}

// PushArgv is the oras command line that keeps a file, saved as "content"
// in its working directory, whose SHA-256 is sum.
func (r *Registry) PushArgv(sum string) []string {
	argv := []string{"push", "--format", "json", "--artifact-type", FileType}
	if r.PlainHTTP {
		argv = append(argv, "--plain-http")
	}
	return append(argv, r.Repo+":sha256-"+sum, "content:application/octet-stream")
}

// push keeps the file at path (named "content"), whose SHA-256 is sum, and
// checks that the registry holds it under that digest.
func (r *Registry) push(ctx context.Context, path, sum string) error {
	if filepath.Base(path) != "content" {
		return fmt.Errorf("pirn: registry: %s isn't named content", path)
	}
	cmd := exec.CommandContext(ctx, r.oras(), r.PushArgv(sum)...)
	cmd.Dir = filepath.Dir(path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("oras push: %v: %s", err, clip(strings.TrimSpace(stderr.String()), 400))
	}
	var m struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil || m.Reference == "" {
		return fmt.Errorf("oras push: unexpected output %q", clip(stdout.String(), 200))
	}
	return nil
}

// FetchCommand is the command that writes a kept file back out, to file.
func FetchCommand(repo, sum, file string) string {
	return fmt.Sprintf("oras blob fetch --output %s %s@sha256:%s", shellQuote(file), repo, sum)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tempContent makes a fresh directory for one file and opens "content"
// in it for writing; done removes the directory.
func tempContent() (path string, f io.WriteCloser, done func(), err error) {
	dir, err := os.MkdirTemp("", "pirn-large-")
	if err != nil {
		return "", nil, nil, err
	}
	path = filepath.Join(dir, "content")
	file, err := os.Create(path)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, nil, err
	}
	return path, file, func() { file.Close(); os.RemoveAll(dir) }, nil
}
