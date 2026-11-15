package cli

// TestMain isolates the credential store for every cli test: a dev machine
// with a real ~/.banyan/credentials.json must never leak its session into
// unit-test clients (the store is consulted at newClient time).
import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "banyan-cli-test-*")
	if err != nil {
		os.Exit(1)
	}
	os.Setenv("BANYAN_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
