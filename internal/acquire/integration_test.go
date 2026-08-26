package acquire

import (
	"context"
	"os"
	"testing"
)

// This opt-in test exercises the pinned public package and trust chain without
// making ordinary unit test runs depend on the network.
func TestOfficialCOFFLoaderIntegration(t *testing.T) {
	if os.Getenv("SLIVER_BOF_E2E_NETWORK_TEST") != "1" {
		t.Skip("set SLIVER_BOF_E2E_NETWORK_TEST=1 to test the official signed package")
	}
	for _, architecture := range []string{"386", "amd64"} {
		t.Run(architecture, func(t *testing.T) {
			loader, err := COFFLoader(context.Background(), Target{OS: "windows", Arch: architecture}, COFFLoaderOptions{
				CacheDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if loader.Export != coffLoaderExport || loader.Digest != pinnedLoaderDigest(architecture) || !loader.Provenance.Verified {
				t.Fatalf("unexpected official loader: %+v", loader)
			}
		})
	}
}
