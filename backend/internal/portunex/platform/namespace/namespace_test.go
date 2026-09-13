package namespace

import (
	"errors"
	"testing"
)

func TestSpaceSeparatesInstancesFamiliesAndResourceTypes(t *testing.T) {
	first, err := New("recovery-a")
	if err != nil {
		t.Fatalf("New(recovery-a) error = %v", err)
	}
	second, err := New("recovery-b")
	if err != nil {
		t.Fatalf("New(recovery-b) error = %v", err)
	}

	firstKey, err := first.Key("session", "42")
	if err != nil {
		t.Fatalf("first.Key() error = %v", err)
	}
	secondKey, err := second.Key("session", "42")
	if err != nil {
		t.Fatalf("second.Key() error = %v", err)
	}
	otherFamily, err := first.Key("user", "42")
	if err != nil {
		t.Fatalf("first.Key(other family) error = %v", err)
	}
	objectID, err := first.ObjectID("session", "42")
	if err != nil {
		t.Fatalf("first.ObjectID() error = %v", err)
	}
	channel, err := first.Channel("session")
	if err != nil {
		t.Fatalf("first.Channel() error = %v", err)
	}
	lock, err := first.Lock("session")
	if err != nil {
		t.Fatalf("first.Lock() error = %v", err)
	}

	if firstKey != "portunex:v1:recovery-a:key:session:42" {
		t.Fatalf("first.Key() = %q", firstKey)
	}
	values := []string{firstKey, secondKey, otherFamily, objectID, channel, lock}
	for left := range values {
		for right := left + 1; right < len(values); right++ {
			if values[left] == values[right] {
				t.Fatalf("namespace collision between %q and %q", values[left], values[right])
			}
		}
	}
}

func TestSpacePreservesTextualIDsWithoutNumericNormalization(t *testing.T) {
	space, err := New("recovery-20260913")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	zeroPadded, err := space.Key("user", "01")
	if err != nil {
		t.Fatalf("Key(01) error = %v", err)
	}
	plain, err := space.Key("user", "1")
	if err != nil {
		t.Fatalf("Key(1) error = %v", err)
	}
	if zeroPadded == plain {
		t.Fatalf("numeric-looking IDs collided: %q", zeroPadded)
	}
}

func TestSpaceRejectsEmptyAndDelimiterInjection(t *testing.T) {
	cases := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "empty instance",
			call: func() error {
				_, err := New("")
				return err
			},
			want: ErrInvalidSpace,
		},
		{
			name: "instance delimiter",
			call: func() error {
				_, err := New("recovery:other")
				return err
			},
			want: ErrInvalidSpace,
		},
		{
			name: "family delimiter",
			call: func() error {
				space, err := New("recovery")
				if err != nil {
					return err
				}
				_, err = space.Key("user:admin", "42")
				return err
			},
			want: ErrInvalidComponent,
		},
		{
			name: "empty id",
			call: func() error {
				space, err := New("recovery")
				if err != nil {
					return err
				}
				_, err = space.ObjectID("user", "")
				return err
			},
			want: ErrInvalidComponent,
		},
		{
			name: "channel delimiter",
			call: func() error {
				space, err := New("recovery")
				if err != nil {
					return err
				}
				_, err = space.Channel("events:all")
				return err
			},
			want: ErrInvalidComponent,
		},
		{
			name: "lock whitespace",
			call: func() error {
				space, err := New("recovery")
				if err != nil {
					return err
				}
				_, err = space.Lock("lease owner")
				return err
			},
			want: ErrInvalidComponent,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.call(); !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, testCase.want)
			}
		})
	}
}

func TestZeroValueSpaceIsRejected(t *testing.T) {
	var space Space
	if _, err := space.Key("user", "42"); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("zero-value Key() error = %v, want ErrInvalidSpace", err)
	}
}
