package actions

import (
	"crypto/sha3"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/release"
)

const legacyBrokenReleaseVersion = "2.1.0.25"

// These hashes identify the official bundles affected by Agent 1.2.3's
// installation layout. The node still verifies the published signatures.
var legacyBrokenReleaseHashes = map[string]string{
	"linux-amd64":  "8d10aa7dd304b3e4f6215d24477084414fbcdd0b006fac8bacac88ff390b910b",
	"linux-arm64":  "5994e657d175c27b53032edf7c88082ae00f20f1424425712d3bec1da1ba440b",
	"darwin-arm64": "b6e9872aef1d19bc9f74602c215d14e71eb68879bc68e020542191c325ac81e2",
}

type LegacyReleaseRecoveryService interface {
	NodeUpdateService
	FailedReleaseStart(unit string) (bool, error)
}

// RecoverLegacyNodeRelease rehomes an affected release using the local bundle.
// Agent 1.2.3 did not record its own version with the node installation, so
// identify the incident by the recorded release, legacy layout and digest.
func RecoverLegacyNodeRelease(state *config.State, binaryPath, unit, platform string, service LegacyReleaseRecoveryService) (bool, error) {
	hash, known := legacyBrokenReleaseHashes[platform]
	if !known {
		return false, nil
	}
	return recoverLegacyNodeRelease(state, binaryPath, unit, release.BaseName(legacyBrokenReleaseVersion, platform), hash, service)
}

func recoverLegacyNodeRelease(state *config.State, binaryPath, unit, publishedName, hash string, service LegacyReleaseRecoveryService) (bool, error) {
	if state == nil || state.ConfigPath == "" || state.NodeSource != nodemanifest.SourceReleases ||
		(state.NodeVersion != legacyBrokenReleaseVersion && state.InstalledNodeVersion != legacyBrokenReleaseVersion) ||
		(state.BinaryPath != "" && state.BinaryPath != binaryPath) ||
		(state.ServiceUnit != "" && state.ServiceUnit != unit) {
		return false, nil
	}
	matches, err := matchesLegacyReleaseLayout(binaryPath, publishedName, hash)
	if err != nil || !matches {
		return false, err
	}
	failed, err := service.FailedReleaseStart(unit)
	if err != nil || !failed {
		return false, err
	}
	matches, err = matchesReleaseHash(binaryPath, hash)
	if err != nil || !matches {
		return false, err
	}
	tx, err := prepareNamedNodeReleaseTransaction(binaryPath, binaryPath, publishedName, fileTransactionOps{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Finalize() }()

	failed, err = service.FailedReleaseStart(unit)
	if err != nil || !failed {
		return false, err
	}
	if err := service.Stop(unit); err != nil {
		return false, err
	}
	rollback := func(err error) (bool, error) {
		return false, withRollbackOutcome(err, func() error {
			// Leave the known-broken legacy service stopped after restoring it.
			return rollbackNodeUpdate(tx, service, unit, false)
		})
	}
	if err := tx.Commit(); err != nil {
		return rollback(err)
	}
	if err := service.Start(unit); err != nil {
		return rollback(err)
	}
	if err := service.CheckReleaseStartup(unit); err != nil {
		return rollback(err)
	}
	return true, nil
}

func matchesLegacyReleaseLayout(binaryPath, publishedName, hash string) (bool, error) {
	node, err := os.Lstat(binaryPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !node.Mode().IsRegular() || node.Mode().Perm()&0o111 == 0 {
		return false, nil
	}
	digestPath := binaryPath + ".dgst"
	wantDigest := fmt.Sprintf("SHA3-256(%s)= %s\n", publishedName, hash)
	info, err := os.Lstat(digestPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(wantDigest)) {
		return false, nil
	}
	digest, err := os.ReadFile(digestPath)
	if err != nil || string(digest) != wantDigest {
		return false, err
	}
	// Each platform shipped these seven Ed448 signatures with 2.1.0.25.
	for _, signer := range []int{1, 2, 3, 10, 11, 13, 17} {
		path := fmt.Sprintf("%s.dgst.sig.%d", binaryPath, signer)
		sig, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !sig.Mode().IsRegular() || sig.Size() != 114 {
			return false, nil
		}
	}
	return true, nil
}

func matchesReleaseHash(path, hash string) (bool, error) {
	binary, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer binary.Close()
	sum := sha3.New256()
	if _, err := io.Copy(sum, binary); err != nil {
		return false, err
	}
	return fmt.Sprintf("%x", sum.Sum(nil)) == hash, nil
}
