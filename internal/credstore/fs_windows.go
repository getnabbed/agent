//go:build windows

package credstore

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PlatformProtector returns DPAPI, which encrypts with a key derived from
// the account the agent runs as. It needs no prompt, works for services
// (including LocalSystem) and binds the file to this machine and account,
// so a copied data directory is useless elsewhere.
func PlatformProtector() Protector { return dpapiProtector{} }

func platformProtectorByName(name string) Protector {
	if name == (dpapiProtector{}).Name() {
		return dpapiProtector{}
	}

	return nil
}

// secureDir creates or checks dir, then applies a protected DACL granting
// full control to SYSTEM, Administrators and the agent's own account only.
//
// The owner is checked first. BUILTIN\Users may create folders under
// %ProgramData%, so a low-privilege user could pre-create the data dir and,
// as its owner, keep control of it whatever DACL we set.
func secureDir(dir string, _ *slog.Logger) error {
	created := false

	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}

		created = true
	}

	me, err := currentUserSID()
	if err != nil {
		return err
	}

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read owner of %s: %w", dir, err)
	}

	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}

	if !owner.Equals(me) && !owner.IsWellKnown(windows.WinLocalSystemSid) && !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return fmt.Errorf("data dir %s is owned by %s, not by this account (%s), SYSTEM or Administrators; remove it or choose another directory",
			dir, owner.String(), me.String())
	}

	if !created {
		if err := checkDedicated(dir); err != nil {
			return err
		}
	}

	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", me.String())

	dsd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}

	dacl, _, err := dsd.DACL()
	if err != nil {
		return err
	}

	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

func currentUserSID() (*windows.SID, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}

	return tu.User.Sid.Copy()
}

// Files inherit the directory's protected DACL; mode bits do not apply.
func checkFileMode(string, *slog.Logger) {}

func isChmodUnsupported(err error) bool { return errors.Is(err, windows.ERROR_NOT_SUPPORTED) }

// Directory fsync is not available on Windows; MoveFileEx is durable enough
// once the file data itself has been flushed.
func syncDir(string) error { return nil }

// renameReplace retries while another process (agent status, antivirus,
// backup software) briefly holds the target open without FILE_SHARE_DELETE.
func renameReplace(from, to string) error {
	var err error

	for attempt := range 20 {
		err = os.Rename(from, to)
		if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}

		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}

	return err
}

type dpapiProtector struct{}

func (dpapiProtector) Name() string { return "dpapi" }

var dpapiEntropy = []byte("nabbed-agent-credentials-v1")

func (dpapiProtector) Protect(plain []byte) ([]byte, error) {
	return dpapi(plain, true)
}

func (dpapiProtector) Unprotect(data []byte) ([]byte, error) {
	return dpapi(data, false)
}

func dpapi(in []byte, protect bool) ([]byte, error) {
	if len(in) == 0 {
		return nil, errors.New("dpapi: empty input")
	}

	inBlob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
	ent := windows.DataBlob{Size: uint32(len(dpapiEntropy)), Data: &dpapiEntropy[0]}

	var (
		out windows.DataBlob
		err error
	)

	if protect {
		err = windows.CryptProtectData(&inBlob, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&inBlob, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}

	if err != nil {
		return nil, err
	}

	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }()

	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
