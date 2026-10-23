package credstore

import "fmt"

// Protector wraps the serialised credentials at rest.
type Protector interface {
	Name() string
	Protect(plain []byte) ([]byte, error)
	Unprotect(data []byte) ([]byte, error)
}

// PlaintextProtector stores data as-is and relies on filesystem permissions.
type PlaintextProtector struct{}

// Name implements Protector.
func (PlaintextProtector) Name() string { return "none" }

// Protect implements Protector.
func (PlaintextProtector) Protect(p []byte) ([]byte, error) { return p, nil }

// Unprotect implements Protector.
func (PlaintextProtector) Unprotect(d []byte) ([]byte, error) { return d, nil }

func protectorByName(name string) (Protector, error) {
	switch name {
	case "none":
		return PlaintextProtector{}, nil
	default:
		if p := platformProtectorByName(name); p != nil {
			return p, nil
		}
		return nil, fmt.Errorf("credentials were protected with %q, which is not available on this platform", name)
	}
}
