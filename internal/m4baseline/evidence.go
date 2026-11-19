package m4baseline

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type SourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type SourceIdentity struct {
	Commit string       `json:"commit"`
	Branch string       `json:"branch"`
	Dirty  bool         `json:"dirty"`
	Files  []SourceFile `json:"files"`
	SHA256 string       `json:"source_sha256"`
}

func bytesHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func CaptureSource(root string) (SourceIdentity, error) {
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		return cmd.Output()
	}
	var identity SourceIdentity
	commit, err := git("rev-parse", "HEAD")
	if err != nil {
		return identity, fmt.Errorf("source identity: %w", err)
	}
	branch, err := git("branch", "--show-current")
	if err != nil {
		return identity, err
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return identity, err
	}
	paths, err := git("ls-files", "-c", "-o", "--exclude-standard", "-z")
	if err != nil {
		return identity, err
	}
	identity.Commit, identity.Branch = strings.TrimSpace(string(commit)), strings.TrimSpace(string(branch))
	identity.Dirty = len(bytes.TrimSpace(status)) != 0
	seen := make(map[string]bool)
	for _, path := range strings.Split(string(paths), "\x00") {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil || !info.Mode().IsRegular() {
			return identity, errors.New("source file missing or nonregular")
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return identity, err
		}
		identity.Files = append(identity.Files, SourceFile{Path: filepath.ToSlash(path), SHA256: bytesHash(data)})
	}
	if len(identity.Files) == 0 {
		return identity, errors.New("empty source identity")
	}
	sort.Slice(identity.Files, func(i, j int) bool { return identity.Files[i].Path < identity.Files[j].Path })
	encoded, err := json.Marshal(identity.Files)
	if err != nil {
		return identity, err
	}
	identity.SHA256 = bytesHash(encoded)
	return identity, nil
}

// ValidateEvidence checks the same row set that is written to CSV against every summary count and quantile.
func ValidateEvidence(sum Summary, records []ChallengeRecord) error {
	if sum.SchemaVersion != 2 || sum.Overall.Total != len(records) {
		return errors.New("summary row count/schema mismatch")
	}
	byID := make(map[string][]ChallengeRecord)
	for _, row := range records {
		if row.Index != len(byID[row.Generator]) {
			return errors.New("duplicate or missing sample index")
		}
		if row.Solved != (row.Status == "solved") {
			return errors.New("inconsistent sample outcome")
		}
		byID[row.Generator] = append(byID[row.Generator], row)
	}
	for _, result := range sum.Results {
		rows := byID[result.Generator]
		if result.Total != len(rows) || result.Solved != countSolved(rows) {
			return errors.New("generator count mismatch")
		}
		recomputed := summarizeGenerator(result, rows)
		if !reflect.DeepEqual(result, recomputed) {
			return errors.New("generator statistics mismatch")
		}
		delete(byID, result.Generator)
	}
	if len(byID) != 0 || !reflect.DeepEqual(sum.Overall, summarizeOverall(records, sum.Results)) {
		return errors.New("overall statistics mismatch")
	}
	return nil
}

func readEvidenceCSV(data []byte) ([]ChallengeRecord, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}
	indices := make(map[string]int)
	for i, name := range header {
		indices[name] = i
	}
	var records []ChallengeRecord
	for {
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		get := func(name string) string { return fields[indices[name]] }
		var parseErr error
		integer := func(name string) int {
			v, e := strconv.Atoi(get(name))
			if e != nil {
				parseErr = e
			}
			return v
		}
		floating := func(name string) float64 {
			v, e := strconv.ParseFloat(get(name), 64)
			if e != nil {
				parseErr = e
			}
			return v
		}
		boolean := func(name string) bool {
			v, e := strconv.ParseBool(get(name))
			if e != nil {
				parseErr = e
			}
			return v
		}
		unsigned := func(name string) uint64 {
			v, e := strconv.ParseUint(get(name), 10, 64)
			if e != nil {
				parseErr = e
			}
			return v
		}
		row := ChallengeRecord{Generator: get("generator"), Index: integer("index"), Solved: boolean("solved"), ParseFailed: boolean("parse_failed"), GaveUp: boolean("gave_up"),
			SolverCPUMs: floating("solver_cpu_ms"), SolverWallMs: floating("solver_wall_ms"), RSSBytes: unsigned("rss_bytes"), AssetBytes: integer("asset_bytes"), PayloadBytes: integer("payload_bytes"), GenerationAttempts: integer("generation_attempts"), Note: get("note"), Status: get("status"), PublicSHA256: get("public_sha256"), AssetSHA256: get("asset_sha256"), OSCPUStatus: get("os_cpu_status"), OSUserMs: floating("os_user_ms"), OSSystemMs: floating("os_system_ms"), RuntimeEstimateMs: floating("runtime_estimate_user_ms"), GenerationWallMs: floating("generation_wall_ms"), PreparationWallMs: floating("preparation_wall_ms"), GradingWallMs: floating("grading_wall_ms"), HarnessWallMs: floating("harness_wall_ms"), GCCount: uint32(unsigned("gc_count")), GCPauseMs: floating("gc_pause_ms")}
		if parseErr != nil {
			return nil, parseErr
		}
		records = append(records, row)
	}
	return records, nil
}

// PublishEvidence uses an exclusive directory and a final manifest as the bundle commit point.
// Readers must reject a missing/mismatched manifest, including after an interrupted Windows rename.
func PublishEvidence(dir string, sum Summary, records []ChallengeRecord, metadata any, exitCode int) error {
	if err := ValidateEvidence(sum, records); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(dir, ".staging-")
	if err != nil {
		return err
	}
	var csvData, jsonData bytes.Buffer
	if err := WriteCSV(&csvData, records); err != nil {
		return err
	}
	if err := WriteJSON(&jsonData, sum); err != nil {
		return err
	}
	roundTrip, err := readEvidenceCSV(csvData.Bytes())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(roundTrip, records) {
		return errors.New("CSV did not preserve sample records")
	}
	files := map[string][]byte{"m4-results.csv": csvData.Bytes(), "m4-summary.json": jsonData.Bytes()}
	meta, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	files["identity.json"] = meta
	hashes := make(map[string]string)
	for name, data := range files {
		if err := writeSynced(filepath.Join(stage, name), data); err != nil {
			return err
		}
		hashes[name] = bytesHash(data)
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	manifest, err := json.MarshalIndent(map[string]any{"schema_version": 2, "complete": true, "raw_exit_code": exitCode, "files": hashes, "publication": "exclusive directory + manifest commit; hashes unsigned"}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSynced(filepath.Join(stage, "manifest.json"), manifest); err != nil {
		return err
	}
	return os.Rename(filepath.Join(stage, "manifest.json"), filepath.Join(dir, "manifest.json"))
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
