package releaseowner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
	"testing"
)

type installedRecipeFixture struct {
	noEffects
	raw, bound []byte
	bindError  error
}

func (f *installedRecipeFixture) InstalledRuntimeRecipe(hash string) ([]byte, error) {
	if digest(f.raw) != hash {
		return nil, errors.New("unknown recipe")
	}
	return f.raw, nil
}
func (f *installedRecipeFixture) BindInstalledRuntimeRecipe(_ context.Context, hash string, raw []byte) error {
	if digest(raw) != hash || !bytes.Equal(raw, f.raw) {
		return errors.New("wrong original recipe")
	}
	if err := f.bindError; err != nil {
		f.bindError = nil
		return err
	}
	f.bound = bytes.Clone(raw)
	return nil
}

func TestInstalledRecipeTransferRetainsExactOriginalAndRefusesConflicts(t *testing.T) {
	fixture := &installedRecipeFixture{raw: bytes.Repeat([]byte("original approved source"), 30000)}
	s := &session{components: fixture}
	hash := digest(fixture.raw)
	var offset uint64
	for offset < uint64(len(fixture.raw)) {
		reply, err := s.installedRecipe(context.Background(), Request{Op: "read-installed-recipe", RecipeHash: hash, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		chunk := reply.InstalledChunk
		bound, err := s.installedRecipe(context.Background(), Request{Op: "bind-installed-recipe", RecipeHash: hash, Offset: offset, Total: chunk.Total, Content: chunk.Content})
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := base64.StdEncoding.DecodeString(chunk.Content)
		offset += uint64(len(payload))
		if bound.InstalledChunk.Complete != (offset == uint64(len(fixture.raw))) {
			t.Fatal("premature recipe completion")
		}
	}
	if !bytes.Equal(fixture.raw, fixture.bound) || s.installedTransfer != nil {
		t.Fatal("original recipe not restored")
	}
	request := Request{Op: "bind-installed-recipe", RecipeHash: hash, Total: uint64(len(fixture.raw)), Content: base64.StdEncoding.EncodeToString(fixture.raw[:installedRecipeChunkSize])}
	if _, err := s.installedRecipe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.installedRecipe(context.Background(), request); err == nil {
		t.Fatal("overlap admitted")
	}
	request.Offset = installedRecipeChunkSize
	request.RecipeHash = digest([]byte("different"))
	if _, err := s.installedRecipe(context.Background(), request); err == nil {
		t.Fatal("interleaved recipe admitted")
	}
}

func TestInstalledRecipeChunkRejectsNoncanonicalBase64(t *testing.T) {
	for _, content := range []string{"YQ\n==", "YQ\r==", "YR==", "YQ", "YQ==="} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(content, "\n", "newline"), "\r", "return"), func(t *testing.T) {
			f := &installedRecipeFixture{raw: []byte("a")}
			s := &session{components: f}
			_, err := s.installedRecipe(context.Background(), Request{Op: "bind-installed-recipe", RecipeHash: digest(f.raw), Total: 1, Content: content})
			if err == nil || s.installedTransfer != nil || f.bound != nil {
				t.Fatal("noncanonical chunk gained authority")
			}
		})
	}
	f := &installedRecipeFixture{raw: []byte("a")}
	s := &session{components: f}
	if _, err := s.installedRecipe(context.Background(), Request{Op: "bind-installed-recipe", RecipeHash: digest(f.raw), Total: 1, Content: "YQ=="}); err != nil || !bytes.Equal(f.bound, f.raw) {
		t.Fatal("canonical padded chunk refused", err)
	}
}

func sealedRecipeFile(t *testing.T, raw []byte, seals int) *os.File {
	t.Helper()
	fd, err := unix.MemfdCreate("recipe-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "recipe-test")
	t.Cleanup(func() { file.Close() })
	if err := file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	if seals != 0 {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
			t.Fatal(err)
		}
	}
	return file
}

func TestSealedRecipeDescriptorRefusals(t *testing.T) {
	const seals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	raw := []byte("original approved recipe")
	for _, mode := range []string{"unsealed", "partial-seals", "regular-file", "wrong-mode", "empty", "wrong-size", "oversized", "wrong-digest", "foreign-target", "inflight"} {
		t.Run(mode, func(t *testing.T) {
			f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: raw}}
			s := &session{components: f}
			var file *os.File
			switch mode {
			case "unsealed":
				file = sealedRecipeFile(t, raw, 0)
			case "partial-seals":
				file = sealedRecipeFile(t, raw, seals&^unix.F_SEAL_WRITE)
			case "regular-file":
				var err error
				file, err = os.CreateTemp(t.TempDir(), "recipe")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { file.Close() })
				if _, err := file.Write(raw); err != nil {
					t.Fatal(err)
				}
			case "empty":
				file = sealedRecipeFile(t, nil, seals)
			default:
				file = sealedRecipeFile(t, raw, seals)
			}
			r := Request{Op: "bind-sealed-installed-recipe", RecipeHash: digest(raw), Total: uint64(len(raw))}
			switch mode {
			case "wrong-mode":
				if err := file.Chmod(0o640); err != nil {
					t.Fatal(err)
				}
			case "wrong-size":
				r.Total++
			case "oversized":
				r.Total = installedRecipeMaximum + 1
			case "wrong-digest":
				r.RecipeHash = digest([]byte("foreign"))
			case "foreign-target":
				r.Target = &Target{PreparedRecipe: digest([]byte("foreign"))}
			case "inflight":
				s.installedTransfer = &installedRecipeTransfer{hash: r.RecipeHash, total: r.Total}
			}
			if reply, err := s.bindSealedInstalledRecipe(context.Background(), r, file); err == nil || reply.InstalledChunk != nil || f.bound != nil {
				t.Fatal("unsafe recipe descriptor was accepted", err)
			}
		})
	}
}

func TestSealedRecipeRequiresExactlyOneDescriptor(t *testing.T) {
	s, binding, _ := nativeFactFixture(t)
	root, err := IdentityOf(s.root)
	if err != nil {
		t.Fatal(err)
	}
	s.context.Root = root
	request := Request{Op: "bind-sealed-installed-recipe", Session: binding.Session, RecipeHash: digest([]byte("recipe")), Total: 6}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]*os.File{nil, {s.root, s.root}} {
		if _, err := s.handle(context.Background(), raw, files); err == nil {
			t.Fatal("incorrect descriptor count accepted")
		}
	}
}

func TestSealedOwnerRegistrationClearsPriorCompletion(t *testing.T) {
	const seals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	raw := []byte(`{"owner":"owner-registration"}`)
	f := &installedRecipeFixture{raw: raw}
	s := &session{components: f, ownerOrigins: map[string]*invocationObserver{"prior": nil}, ownerCompleted: &Binding{}, ownerCompletedTerminal: "prior", ownerResultsPayload: "prior", ownerResultsRecipe: "prior", ownerResultsSource: "prior"}
	file := sealedRecipeFile(t, raw, seals)
	if _, err := s.bindSealedInstalledRecipe(context.Background(), Request{RecipeHash: digest(raw), Total: uint64(len(raw))}, file); err != nil {
		t.Fatal(err)
	}
	if s.ownerOrigins != nil || s.ownerCompleted != nil || s.ownerCompletedTerminal != "" || s.ownerResultsPayload != "" || s.ownerResultsRecipe != "" || s.ownerResultsSource != "" {
		t.Fatal("prior owner completion was retained")
	}
}

func TestSealedAfterOwnerRequiresOriginalCompletion(t *testing.T) {
	const seals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	raw := []byte("original approved recipe")
	f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: raw}}
	s := &session{components: f}
	hash := digest(raw)
	target := Target{PreparedRecipe: hash, AfterOwner: "v1:runtime:" + hash}
	file := sealedRecipeFile(t, raw, seals)
	if reply, err := s.bindSealedInstalledRecipe(context.Background(), Request{RecipeHash: hash, Total: uint64(len(raw)), Target: &target}, file); err != nil || reply.InstalledChunk == nil || f.target == nil {
		t.Fatal("original after-owner recipe refused", err)
	}
	f.target = nil
	target.AfterOwner = "v1:runtime:" + digest([]byte("different original"))
	if reply, err := s.bindSealedInstalledRecipe(context.Background(), Request{RecipeHash: hash, Total: uint64(len(raw)), Target: &target}, file); err != nil || reply.Code != "unknown" || f.target != nil {
		t.Fatal("missing producer completion admitted", err)
	}
}

func TestSealedRecipeOverNativeTransport(t *testing.T) {
	const seals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	s, binding, _ := nativeFactFixture(t)
	root, _ := IdentityOf(s.root)
	f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: []byte("original approved recipe")}}
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent, child := os.NewFile(uintptr(sockets[0]), "recipe peer"), os.NewFile(uintptr(sockets[1]), "recipe native")
	done := make(chan error, 1)
	go func() {
		defer child.Close()
		done <- Serve(t.Context(), child, child, func(context.Context, Context, *os.File) (Components, error) { return f, nil })
	}()
	t.Cleanup(func() {
		parent.Close()
		select {
		case err := <-done:
			if err != nil && !t.Failed() {
				t.Errorf("native drain: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("native drain unavailable")
		}
	})
	c := Context{Session: binding.Session, Artifact: binding.Artifact, ConfigHome: s.root.Name(), DataHome: s.root.Name(), RuntimeRoot: s.root.Name(), RepositoryRoot: s.root.Name(), Yard: "default", Target: "target", Root: root, Repository: root}
	peer := nativeTestPeer{t: t, socket: parent}
	peer.request(Request{Op: "initialize", Context: &c}, s.root, s.root)
	hash := digest(f.raw)
	for _, targeted := range []bool{false, true} {
		file := sealedRecipeFile(t, f.raw, seals)
		if _, err := file.WriteAt([]byte("x"), 0); err == nil {
			t.Fatal("sealed recipe remained writable")
		}
		request := Request{Op: "bind-sealed-installed-recipe", Session: binding.Session, RecipeHash: hash, Total: uint64(len(f.raw))}
		if targeted {
			request.Target = &Target{PreparedRecipe: hash, InstanceUUID: "absent"}
		}
		reply := peer.request(request, file)
		if reply.InstalledChunk == nil || !reply.InstalledChunk.Complete || !bytes.Equal(f.bound, f.raw) || targeted && f.target == nil {
			t.Fatal("sealed recipe did not preserve binding")
		}
		if offset, err := file.Seek(0, 1); err != nil || offset != int64(len(f.raw)) {
			t.Fatal("receiver changed the shared descriptor offset", err)
		}
	}
}

type targetRecipeFixture struct {
	installedRecipeFixture
	target *Target
}

func (f *targetRecipeFixture) BindInstalledRuntimeRecipeForTarget(ctx context.Context, hash string, raw []byte, target Target) error {
	if err := f.BindInstalledRuntimeRecipe(ctx, hash, raw); err != nil {
		return err
	}
	f.target = &target
	return nil
}
func TestInstalledRecipeOriginalTargetOnlyOnFinalChunk(t *testing.T) {
	f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: bytes.Repeat([]byte("x"), installedRecipeChunkSize+3)}}
	hash := digest(f.raw)
	target := Target{PreparedRecipe: hash, InstanceUUID: "absent", Owner: "original-owner", Source: digest([]byte("source"))}
	request := Request{Op: "bind-installed-recipe", RecipeHash: hash, Total: uint64(len(f.raw)), Content: base64.StdEncoding.EncodeToString(f.raw[:installedRecipeChunkSize]), Target: &target}
	s := &session{components: f}
	if _, err := s.installedRecipe(context.Background(), request); err == nil || s.installedTransfer != nil || f.target != nil {
		t.Fatal("early target gained authority")
	}
	request.Target = nil
	if _, err := s.installedRecipe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Offset = installedRecipeChunkSize
	request.Content = base64.StdEncoding.EncodeToString(f.raw[installedRecipeChunkSize:])
	request.Target = &target
	if _, err := s.installedRecipe(context.Background(), request); err != nil || f.target == nil || *f.target != target || !bytes.Equal(f.bound, f.raw) {
		t.Fatal("final original target/body lost", err)
	}
	wrong := target
	wrong.PreparedRecipe = digest([]byte("foreign"))
	request.Offset = 0
	request.Target = &wrong
	if _, err := s.installedRecipe(context.Background(), request); err == nil {
		t.Fatal("foreign recipe target accepted")
	}
}

func TestInstalledRecipeTargetPassesStrictSessionAdmission(t *testing.T) {
	s, b, _ := nativeFactFixture(t)
	root, err := IdentityOf(s.root)
	if err != nil {
		t.Fatal(err)
	}
	s.context.Root = root
	f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: []byte("original body")}}
	s.components = f
	hash := digest(f.raw)
	target := Target{PreparedRecipe: hash, InstanceUUID: "absent"}
	raw, _ := json.Marshal(Request{Op: "bind-installed-recipe", Session: b.Session, RecipeHash: hash, Total: uint64(len(f.raw)), Content: base64.StdEncoding.EncodeToString(f.raw), Target: &target})
	if reply, err := s.handle(context.Background(), raw, nil); err != nil || reply.InstalledChunk == nil || !reply.InstalledChunk.Complete || f.target == nil || *f.target != target {
		t.Fatal("final target refused at strict admission", err)
	}
	f.target = nil
	raw, _ = json.Marshal(Request{Op: "bind-installed-recipe", Session: b.Session, RecipeHash: hash, Total: uint64(len(f.raw) + 1), Content: base64.StdEncoding.EncodeToString(f.raw), Target: &target})
	if _, err = s.handle(context.Background(), raw, nil); err == nil || f.target != nil || s.installedTransfer != nil {
		t.Fatal("nonfinal target passed strict admission")
	}
	s.components = noEffects{}
	if _, err = s.handle(context.Background(), raw, nil); err == nil {
		t.Fatal("absent installed recipe capability became component refusal")
	}
}

func TestInstalledRecipeComponentRefusalPreservesFramedSession(t *testing.T) {
	for _, mode := range []string{"stale", "public", "target-stale", "target-public"} {
		t.Run(mode, func(t *testing.T) {
			s, b, _ := nativeFactFixture(t)
			cause := error(domain.ErrPlanStale)
			code := "stale"
			if strings.HasSuffix(mode, "public") {
				cause, code = publicComponentFailure{message: "original component unavailable"}, "unavailable"
			}
			f := &targetRecipeFixture{installedRecipeFixture: installedRecipeFixture{raw: []byte("original body"), bindError: cause}}
			sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent, child := os.NewFile(uintptr(sockets[0]), "recipe peer"), os.NewFile(uintptr(sockets[1]), "recipe native")
			done := make(chan error, 1)
			go func() {
				defer child.Close()
				done <- Serve(t.Context(), child, child, func(context.Context, Context, *os.File) (Components, error) { return f, nil })
			}()
			t.Cleanup(func() {
				parent.Close()
				select {
				case err := <-done:
					if err != nil && !t.Failed() {
						t.Errorf("native drain type=%T", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("native drain unavailable")
				}
			})
			root, _ := IdentityOf(s.root)
			value := Context{Session: b.Session, Artifact: b.Artifact, ConfigHome: s.root.Name(), DataHome: s.root.Name(), RuntimeRoot: s.root.Name(), RepositoryRoot: s.root.Name(), Yard: "default", Target: "target", Root: root, Repository: root}
			peer := nativeTestPeer{t: t, socket: parent}
			peer.request(Request{Op: "initialize", Context: &value}, s.root, s.root)
			request := Request{Op: "bind-installed-recipe", Session: b.Session, RecipeHash: digest(f.raw), Total: uint64(len(f.raw)), Content: base64.StdEncoding.EncodeToString(f.raw)}
			if strings.HasPrefix(mode, "target-") {
				request.Target = &Target{PreparedRecipe: request.RecipeHash, InstanceUUID: "absent"}
			}
			var reply Reply
			if err := strict(peer.frame(15, request), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Code != code || reply.InstalledChunk != nil || (reply.Diagnostic != nil) != strings.HasSuffix(mode, "public") {
				t.Fatal("component refusal lost classification")
			}
			read := peer.request(Request{Op: "read-installed-recipe", Session: b.Session, RecipeHash: request.RecipeHash})
			if read.InstalledChunk == nil || read.InstalledChunk.Content != request.Content {
				t.Fatal("refusal poisoned readonly session")
			}
			retry := peer.request(request)
			if retry.InstalledChunk == nil || !retry.InstalledChunk.Complete {
				t.Fatal("refusal retained incomplete transfer")
			}
		})
	}
}
