//go:build windows

package credstore

import (
	"errors"
	"log/slog"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerOnlySDDL grants full control to SYSTEM, Administrators and the
// object's owner, inherited by children, with inheritance from the parent
// blocked (P). Everyone else, including other interactive users, gets
// nothing.
const ownerOnlySDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)"

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

func secureDir(dir string, _ *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(ownerOnlySDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

// Files inherit the directory's protected DACL; mode bits do not apply.
func checkFileMode(string, *slog.Logger) {}

func isChmodUnsupported(err error) bool { return errors.Is(err, windows.ERROR_NOT_SUPPORTED) }

// Directory fsync is not available on Windows; MoveFileEx is durable enough
// once the file data itself has been flushed.
func syncDir(string) error { return nil }

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
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&inBlob, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&inBlob, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
