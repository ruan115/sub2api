//go:build portunex_integration && (darwin || linux)

package testcluster

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestRealClusterIdentityAndClose(t *testing.T) {
	c := Start(t)
	if err := c.checkConnection(context.Background(), c.DB, c.database); err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := c.DB.QueryRow("SELECT marker FROM portunex_recovery_meta.instance").Scan(&marker); err != nil || marker != c.marker {
		t.Fatal("marker mismatch")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.done:
	default:
		t.Fatal("server not reaped")
	}
	if _, err := os.Stat(c.root); !os.IsNotExist(err) {
		t.Fatal("synthetic cluster directory retained after clean stop")
	}
}

func TestAbnormalBootstrapOrShutdownRetainsOwnedDirectory(t *testing.T) {
	for _, stage := range []string{"bootstrap failed", "forced shutdown", "unexpected postmaster exit"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			failure := errors.New(stage)
			c := &Cluster{root: root, closeErr: failure}
			if err := c.Close(); !errors.Is(err, failure) {
				t.Fatal("failure was hidden")
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatal("abnormal path removed owned data")
			}
			if err := c.Close(); !errors.Is(err, failure) {
				t.Fatal("repeated Close hid failure")
			}
		})
	}
}

func TestSocketDialerCannotUseNetworkOrAnotherSocket(t *testing.T) {
	d := socketDialer{"/private/synthetic/.s.PGSQL.5432"}
	for _, test := range [][2]string{{"tcp", "127.0.0.1:5432"}, {"tcp", "216.106.185.119:5432"}, {"unix", "/tmp/.s.PGSQL.5432"}} {
		if _, err := d.Dial(test[0], test[1]); err != errIsolation {
			t.Fatal("foreign target not rejected before dial")
		}
	}
}
