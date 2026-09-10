package actions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type fileTransactionCandidate struct {
	source      string
	destination string
	mode        os.FileMode
	binary      bool
}

type preparedFileTransactionCandidate struct {
	staged      string
	destination string
	binary      bool
}

type fileTransactionBackup struct {
	staged      string
	destination string
	mode        os.FileMode
	binary      bool
}

type fileTransactionOps struct {
	copy      func(string, string, os.FileMode) error
	rename    func(string, string) error
	remove    func(string) error
	removeAll func(string) error
}

func defaultFileTransactionOps() fileTransactionOps {
	return fileTransactionOps{
		copy:      copyFile,
		rename:    os.Rename,
		remove:    os.Remove,
		removeAll: os.RemoveAll,
	}
}

func (ops fileTransactionOps) withDefaults() fileTransactionOps {
	defaults := defaultFileTransactionOps()
	if ops.copy == nil {
		ops.copy = defaults.copy
	}
	if ops.rename == nil {
		ops.rename = defaults.rename
	}
	if ops.remove == nil {
		ops.remove = defaults.remove
	}
	if ops.removeAll == nil {
		ops.removeAll = defaults.removeAll
	}
	return ops
}

type fileTransaction struct {
	root              string
	binaryDestination string
	candidates        []preparedFileTransactionCandidate
	managed           []string
	backups           []fileTransactionBackup
	ops               fileTransactionOps
	finalized         bool
}

func prepareFileTransaction(candidates []fileTransactionCandidate, managed []string, ops fileTransactionOps) (*fileTransaction, error) {
	if len(candidates) == 0 {
		return nil, errors.New("file transaction requires at least one candidate")
	}
	ops = ops.withDefaults()

	binaryDestination := ""
	for _, candidate := range candidates {
		if candidate.binary {
			if binaryDestination != "" {
				return nil, errors.New("file transaction has multiple binary candidates")
			}
			binaryDestination = candidate.destination
		}
	}
	if binaryDestination == "" {
		binaryDestination = candidates[len(candidates)-1].destination
	}

	parent := filepath.Dir(binaryDestination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create transaction parent: %w", err)
	}
	root, err := os.MkdirTemp(parent, ".quilscan-file-transaction-*")
	if err != nil {
		return nil, fmt.Errorf("create file transaction: %w", err)
	}
	tx := &fileTransaction{
		root:              root,
		binaryDestination: binaryDestination,
		ops:               ops,
	}
	fail := func(err error) (*fileTransaction, error) {
		_ = ops.removeAll(root)
		return nil, err
	}

	managed = uniquePaths(append(managed, candidateDestinations(candidates)...))
	tx.managed = managed

	for index, candidate := range candidates {
		info, err := os.Stat(candidate.source)
		if err != nil {
			return fail(fmt.Errorf("stat candidate %s: %w", candidate.source, err))
		}
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("candidate %s is not a regular file", candidate.source))
		}
		mode := candidate.mode
		if mode == 0 {
			mode = info.Mode().Perm()
		}
		staged := filepath.Join(root, fmt.Sprintf("candidate-%03d", index))
		if err := ops.copy(candidate.source, staged, mode); err != nil {
			return fail(fmt.Errorf("stage candidate %s: %w", candidate.source, err))
		}
		tx.candidates = append(tx.candidates, preparedFileTransactionCandidate{
			staged:      staged,
			destination: candidate.destination,
			binary:      candidate.binary,
		})
	}

	for index, destination := range managed {
		info, err := os.Stat(destination)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("stat live file %s: %w", destination, err))
		}
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("live file %s is not a regular file", destination))
		}
		staged := filepath.Join(root, fmt.Sprintf("backup-%03d", index))
		if err := ops.copy(destination, staged, info.Mode().Perm()); err != nil {
			return fail(fmt.Errorf("back up %s: %w", destination, err))
		}
		tx.backups = append(tx.backups, fileTransactionBackup{
			staged:      staged,
			destination: destination,
			mode:        info.Mode().Perm(),
			binary:      destination == binaryDestination,
		})
	}
	return tx, nil
}

func (tx *fileTransaction) Commit() error {
	if tx == nil {
		return errors.New("file transaction is nil")
	}
	if tx.finalized {
		return errors.New("file transaction is finalized")
	}

	for _, candidate := range tx.candidates {
		if candidate.binary {
			continue
		}
		if err := tx.ops.rename(candidate.staged, candidate.destination); err != nil {
			return fmt.Errorf("commit %s: %w", candidate.destination, err)
		}
	}

	destinations := make(map[string]struct{}, len(tx.candidates))
	for _, candidate := range tx.candidates {
		destinations[filepath.Clean(candidate.destination)] = struct{}{}
	}
	for _, path := range tx.managed {
		clean := filepath.Clean(path)
		if clean == filepath.Clean(tx.binaryDestination) {
			continue
		}
		if _, keep := destinations[clean]; keep {
			continue
		}
		if err := tx.ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove obsolete %s: %w", path, err)
		}
	}

	for _, candidate := range tx.candidates {
		if !candidate.binary {
			continue
		}
		if err := tx.ops.rename(candidate.staged, candidate.destination); err != nil {
			return fmt.Errorf("commit %s: %w", candidate.destination, err)
		}
	}
	return nil
}

func (tx *fileTransaction) Rollback() error {
	if tx == nil {
		return errors.New("file transaction is nil")
	}
	if tx.finalized {
		return errors.New("file transaction is finalized")
	}

	var rollbackErrors []error
	for _, path := range uniquePaths(append(append([]string{}, tx.managed...), preparedCandidateDestinations(tx.candidates)...)) {
		if err := tx.ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove replacement %s: %w", path, err))
		}
	}

	restore := func(backup fileTransactionBackup) {
		staged := backup.staged + ".restore"
		if err := tx.ops.copy(backup.staged, staged, backup.mode); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("stage restore %s: %w", backup.destination, err))
			return
		}
		if err := tx.ops.rename(staged, backup.destination); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %s: %w", backup.destination, err))
			_ = tx.ops.remove(staged)
		}
	}
	for _, backup := range tx.backups {
		if !backup.binary {
			restore(backup)
		}
	}
	for _, backup := range tx.backups {
		if backup.binary {
			restore(backup)
		}
	}
	return errors.Join(rollbackErrors...)
}

func (tx *fileTransaction) Finalize() error {
	if tx == nil || tx.finalized {
		return nil
	}
	tx.finalized = true
	return tx.ops.removeAll(tx.root)
}

type rollbackOutcomeError struct {
	operation error
	rollback  error
}

func (e *rollbackOutcomeError) Error() string {
	if e.rollback == nil {
		return fmt.Sprintf("%v; rollback succeeded", e.operation)
	}
	return fmt.Sprintf("%v; rollback failed: %v", e.operation, e.rollback)
}

func (e *rollbackOutcomeError) Unwrap() []error {
	if e.rollback == nil {
		return []error{e.operation}
	}
	return []error{e.operation, e.rollback}
}

func withRollbackOutcome(operation error, rollback func() error) error {
	if operation == nil {
		return nil
	}
	var rollbackErr error
	if rollback != nil {
		rollbackErr = rollback()
	}
	return &rollbackOutcomeError{operation: operation, rollback: rollbackErr}
}

func prepareNodeReleaseTransaction(src, dst string, ops fileTransactionOps) (*fileTransaction, error) {
	digest := src + ".dgst"
	if _, err := os.Stat(digest); err != nil {
		return nil, fmt.Errorf("stat digest: %w", err)
	}
	signatures, err := filepath.Glob(src + ".dgst.sig.*")
	if err != nil {
		return nil, fmt.Errorf("glob signatures: %w", err)
	}
	if len(signatures) == 0 {
		return nil, fmt.Errorf("no signature files found for %s", filepath.Base(src))
	}
	sort.Strings(signatures)

	candidates := []fileTransactionCandidate{{
		source: digest, destination: dst + ".dgst", mode: 0o644,
	}}
	for _, signature := range signatures {
		candidates = append(candidates, fileTransactionCandidate{
			source:      signature,
			destination: dst + signature[len(src):],
			mode:        0o644,
		})
	}
	candidates = append(candidates, fileTransactionCandidate{
		source: src, destination: dst, mode: 0o755, binary: true,
	})
	managed, err := nodeManagedPaths(dst)
	if err != nil {
		return nil, err
	}
	return prepareFileTransaction(candidates, managed, ops)
}

func prepareDevNodeTransaction(src, dst string, ops fileTransactionOps) (*fileTransaction, error) {
	managed, err := nodeManagedPaths(dst)
	if err != nil {
		return nil, err
	}
	return prepareFileTransaction([]fileTransactionCandidate{{
		source: src, destination: dst, mode: 0o755, binary: true,
	}}, managed, ops)
}

func prepareQClientTransaction(src, dst string, ops fileTransactionOps) (*fileTransaction, error) {
	for _, suffix := range []string{".dgst", ".sig"} {
		if _, err := os.Stat(src + suffix); err != nil {
			return nil, fmt.Errorf("stat qclient%s: %w", suffix, err)
		}
	}
	candidates := []fileTransactionCandidate{
		{source: src + ".dgst", destination: dst + ".dgst", mode: 0o644},
		{source: src + ".sig", destination: dst + ".sig", mode: 0o644},
	}
	signatures, err := filepath.Glob(src + ".dgst.sig.*")
	if err != nil {
		return nil, fmt.Errorf("glob qclient signatures: %w", err)
	}
	sort.Strings(signatures)
	for _, signature := range signatures {
		candidates = append(candidates, fileTransactionCandidate{
			source:      signature,
			destination: dst + signature[len(src):],
			mode:        0o644,
		})
	}
	candidates = append(candidates, fileTransactionCandidate{
		source: src, destination: dst, mode: 0o755, binary: true,
	})
	managed, err := qclientManagedPaths(dst)
	if err != nil {
		return nil, err
	}
	return prepareFileTransaction(candidates, managed, ops)
}

func nodeManagedPaths(binaryPath string) ([]string, error) {
	signatures, err := filepath.Glob(binaryPath + ".dgst.sig.*")
	if err != nil {
		return nil, fmt.Errorf("glob installed node signatures: %w", err)
	}
	return uniquePaths(append([]string{binaryPath, binaryPath + ".dgst"}, signatures...)), nil
}

func qclientManagedPaths(binaryPath string) ([]string, error) {
	signatures, err := filepath.Glob(binaryPath + ".dgst.sig.*")
	if err != nil {
		return nil, fmt.Errorf("glob installed qclient signatures: %w", err)
	}
	return uniquePaths(append([]string{binaryPath, binaryPath + ".dgst", binaryPath + ".sig"}, signatures...)), nil
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if clean == "." || clean == "" {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func candidateDestinations(candidates []fileTransactionCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.destination)
	}
	return out
}

func preparedCandidateDestinations(candidates []preparedFileTransactionCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.destination)
	}
	return out
}
