package pirn_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/pirn"
)

// TestInv_RegistryIsCheckedBeforeAnyContainer: a registry repository that
// isn't host/path in lower case, without a tag or digest, fails the step
// with ErrSetup before any container starts; PushArgv is exactly the oras
// command the docs give.
func TestInv_RegistryIsCheckedBeforeAnyContainer(t *testing.T) {
	podman, log := fakePodman(t, "")
	for _, repo := range []string{"", "ghcr.io", "ghcr.io/Acme/files", "ghcr.io/acme/files:v1", "ghcr.io/acme/files@sha256:00",
		"-ghcr.io/acme", "ghcr.io/acme/files\n", "ghcr.io/acme files", "https://ghcr.io/acme"} {
		a := pirn.Agent{Claude: pirn.Claude{APIKey: "k"}, Runner: bobbin.Runner{Podman: podman}, Image: zeros, Registry: &pirn.Registry{Repo: repo}}
		if _, err := a.Run(context.Background(), pirn.Question{Instruction: "x"}); !errors.Is(err, pirn.ErrSetup) {
			t.Errorf("repo %q: %v", repo, err)
		}
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("podman ran: %s", b)
	}
	r := pirn.Registry{Repo: "localhost:5000/acme/files", PlainHTTP: true}
	want := []string{"push", "--format", "json", "--artifact-type", "application/vnd.heedle.file", "--plain-http",
		"localhost:5000/acme/files:sha256-ab", "content:application/octet-stream"}
	if got := r.PushArgv("ab"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q", got)
	}
	if got := pirn.FetchCommand("ghcr.io/acme/files", "ab", "my report's.bin"); got != `oras blob fetch --output 'my report'\''s.bin' ghcr.io/acme/files@sha256:ab` {
		t.Errorf("fetch %s", got)
	}
}

// fakeOras writes an oras that logs its arguments and the file it pushes,
// and answers with exit, or as a real push does.
func fakeOras(t *testing.T, fail bool) (string, string) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	sh := `printf '%s\n' "$*" >> ` + log + `; wc -c < content >> ` + log + `
`
	if fail {
		sh += `echo 'Error: unauthorized: authentication required' >&2; exit 1`
	} else {
		sh += `echo '{"reference":"r@sha256:00","digest":"sha256:00"}'`
	}
	p := filepath.Join(dir, "oras")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+sh+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, log
}

// bigPodman is a fake podman whose /work/out holds small.txt (3 bytes)
// and big.bin (300 KB, too big for the record).
func bigPodman(t *testing.T) (string, string) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	sh := `printf '%s\n' "$*" >> ` + log + `
case "$1" in
run) echo id;;
diff) echo '{}';;
exec) cat >/dev/null
  case "$*" in
  *"find . -type f -print0"*) printf './big.bin\0./small.txt\0';;
  *"/work/out/big.bin") head -c 300000 /dev/zero;;
  *"/work/out/small.txt") printf abc;;
  esac;;
esac`
	p := filepath.Join(dir, "podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+sh+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, log
}

// TestInv_LargeFilesGoToTheRegistry: a file too big for the record is
// copied out whole, pushed with oras under its SHA-256, and recorded with
// its size, digest and repository, never its bytes; the step's leftover
// processes are stopped before /work/out is read; and a registry that
// refuses the file leaves it listed as skipped, with oras's reason,
// without failing the step.
func TestInv_LargeFilesGoToTheRegistry(t *testing.T) {
	api := newScript(t, turn("end_turn", `{"type":"text","text":"done"}`))
	podman, plog := bigPodman(t)
	oras, olog := fakeOras(t, false)
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: api.srv.URL}, Runner: bobbin.Runner{Podman: podman}, Image: zeros,
		Registry: &pirn.Registry{Oras: oras, Repo: "localhost:5000/acme/files"}}
	res, err := a.Run(context.Background(), pirn.Question{Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(make([]byte, 300000))
	hexSum := hex.EncodeToString(sum[:])
	want := []pirn.File{{Path: "big.bin", Size: 300000, OCI: "localhost:5000/acme/files", SHA256: hexSum}, {Path: "small.txt", Size: 3, Text: "abc"}}
	if !reflect.DeepEqual(res.Files, want) || len(res.Skipped) != 0 {
		t.Errorf("files %+v, skipped %v", res.Files, res.Skipped)
	}
	pushed, _ := os.ReadFile(olog)
	if string(pushed) != "push --format json --artifact-type application/vnd.heedle.file localhost:5000/acme/files:sha256-"+hexSum+" content:application/octet-stream\n300000\n" {
		t.Errorf("oras ran: %q", pushed)
	}
	calls, _ := os.ReadFile(plog)
	stop, find := strings.Index(string(calls), "kill -9 -1"), strings.Index(string(calls), "find . -type f")
	if stop < 0 || stop > find {
		t.Errorf("leftover processes weren't stopped before /work/out was read:\n%s", calls)
	}

	refusing, _ := fakeOras(t, true)
	a.Registry = &pirn.Registry{Oras: refusing, Repo: "localhost:5000/acme/files"}
	res, err = a.Run(context.Background(), pirn.Question{Instruction: "x"})
	if err != nil || len(res.Files) != 1 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "big.bin: the registry refused it") ||
		!strings.Contains(res.Skipped[0], "unauthorized") {
		t.Errorf("err %v, files %+v, skipped %q", err, res.Files, res.Skipped)
	}
}

// registry starts the distribution project's registry on a free local
// port and returns its address, or skips without it (fails in CI).
func registry(t *testing.T) (oras, addr string) {
	t.Helper()
	oras, err1 := exec.LookPath("oras")
	reg, err2 := exec.LookPath("registry")
	if err1 != nil || err2 != nil {
		if os.Getenv("PIRN_REQUIRE_PODMAN") == "1" {
			t.Fatalf("oras or registry isn't installed: %v %v", err1, err2)
		}
		t.Skip("oras or registry isn't installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	l.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: %s\nhttp:\n  addr: %s\n", filepath.Join(dir, "data"), addr)), 0o644)
	cmd := exec.Command(reg, "serve", cfg)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return oras, addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the registry didn't start")
	return "", ""
}

// TestContainer_AgentKeepsLargeFilesInARegistry: with a real container and
// a real registry, a file too big for the record is kept in the registry,
// and the fetch command the docs give returns it byte for byte.
func TestContainer_AgentKeepsLargeFilesInARegistry(t *testing.T) {
	img := shellImage(t)
	oras, addr := registry(t)
	api := newScript(t,
		turn("tool_use", toolUse("t1", "head -c 3000000 /dev/urandom > out/big.bin && sha256sum out/big.bin && echo hi > out/small.txt")),
		turn("end_turn", `{"type":"text","text":"done"}`))
	a := pirn.Agent{Claude: pirn.Claude{APIKey: "k", BaseURL: api.srv.URL}, Image: img,
		Registry: &pirn.Registry{Oras: oras, Repo: addr + "/heedle/files", PlainHTTP: true}}
	res, err := a.Run(context.Background(), pirn.Question{Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	inside := strings.Fields(res.Commands[0].Stdout)
	if len(res.Files) != 2 || res.Files[0].Path != "big.bin" || res.Files[0].Size != 3000000 || len(inside) == 0 || res.Files[0].SHA256 != inside[0] ||
		res.Files[0].Text != "" || res.Files[0].Base64 != "" || res.Files[1].Text != "hi\n" {
		t.Fatalf("files %+v; inside %q; skipped %v", res.Files, inside, res.Skipped)
	}
	out := filepath.Join(t.TempDir(), "big.bin")
	fetch := pirn.FetchCommand(res.Files[0].OCI, res.Files[0].SHA256, out)
	if b, err := exec.Command("sh", "-c", strings.Replace(fetch, "oras blob fetch", oras+" blob fetch --plain-http", 1)).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", fetch, err, b)
	}
	b, _ := os.ReadFile(out)
	if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != res.Files[0].SHA256 {
		t.Error("the fetched file differs")
	}
}
