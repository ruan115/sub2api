// Package namespace builds isolated Portunex recovery identifiers.
package namespace

import (
	"errors"
	"fmt"
	"strings"
)

const (
	productPrefix = "portunex"
	versionPrefix = "v1"

	maxInstanceLength  = 63
	maxComponentLength = 128
)

var (
	// ErrInvalidSpace reports an invalid recovery instance name.
	ErrInvalidSpace = errors.New("invalid Portunex namespace space")

	// ErrInvalidComponent reports an invalid key, channel, lock, or object part.
	ErrInvalidComponent = errors.New("invalid Portunex namespace component")
)

// Space is an immutable recovery namespace. Construct it with New.
type Space struct {
	instance string
}

// New creates a namespace isolated by instance. The instance is intentionally
// not normalized: callers choose the exact, non-secret recovery identifier.
func New(instance string) (Space, error) {
	if err := validate(instance, maxInstanceLength); err != nil {
		return Space{}, fmt.Errorf("%w: instance %s", ErrInvalidSpace, err)
	}
	return Space{instance: instance}, nil
}

// Key returns a namespaced storage key for one family and raw textual ID.
func (s Space) Key(family, id string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := validateNamed("family", family); err != nil {
		return "", err
	}
	if err := validateNamed("id", id); err != nil {
		return "", err
	}
	return s.compose("key", family, id), nil
}

// Channel returns a namespaced publish/subscribe channel. Redis logical DBs do
// not isolate Pub/Sub, so the full channel name carries the recovery space.
func (s Space) Channel(topic string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := validateNamed("topic", topic); err != nil {
		return "", err
	}
	return s.compose("channel", topic), nil
}

// Lock returns a namespaced distributed-lock name.
func (s Space) Lock(name string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := validateNamed("lock name", name); err != nil {
		return "", err
	}
	return s.compose("lock", name), nil
}

// ObjectID returns a namespaced opaque object identifier. It preserves the
// textual ID exactly, rather than parsing numeric IDs and merging equivalents.
func (s Space) ObjectID(kind, id string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if err := validateNamed("kind", kind); err != nil {
		return "", err
	}
	if err := validateNamed("id", id); err != nil {
		return "", err
	}
	return s.compose("object", kind, id), nil
}

func (s Space) validate() error {
	if err := validate(s.instance, maxInstanceLength); err != nil {
		return fmt.Errorf("%w: instance %s", ErrInvalidSpace, err)
	}
	return nil
}

func (s Space) compose(parts ...string) string {
	all := make([]string, 0, 3+len(parts))
	all = append(all, productPrefix, versionPrefix, s.instance)
	all = append(all, parts...)
	return strings.Join(all, ":")
}

func validateNamed(label, value string) error {
	if err := validate(value, maxComponentLength); err != nil {
		return fmt.Errorf("%w: %s %s", ErrInvalidComponent, label, err)
	}
	return nil
}

func validate(value string, limit int) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	if len(value) > limit {
		return fmt.Errorf("exceeds %d bytes", limit)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if index == 0 {
			if !isAlphaNumeric(character) {
				return errors.New("must start with an ASCII alphanumeric character")
			}
			continue
		}
		if !isAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return errors.New("contains unsupported characters")
		}
	}
	return nil
}

func isAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}
