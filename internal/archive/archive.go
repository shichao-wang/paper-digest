// Package archive makes offline, self-contained snapshots of a paper library.
// A published directory contains database.db, artifacts/ (the Repository.Root),
// and manifest.json. Restore uses the same layout and never opens state or workers.
package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

const FormatVersion = 1
const manifestLimit = 64 << 20

// Manifest records exactly the snapshot's dependency closure. Paths are canonical
// slash-separated paths relative to the archive, sorted and unique. Hashes cover
// file bytes, including document manifests and identity pointers. This provides
// integrity, not authentication against an attacker who can replace the manifest.
type Manifest struct {
	Format       int    `json:"format"`
	Database     string `json:"database"`
	ArtifactRoot string `json:"artifact_root"`
	Files        []File `json:"files"`
}
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
}

type references struct {
	artifacts map[string]string
	documents map[string][]library.Document
}

// Backup first VACUUMs the store into a private sibling directory. All artifact
// enumeration is then done from that snapshot, never from the changing store.
// targetDir must not exist; a verified manifest is written last before an atomic,
// exclusive rename publishes the directory. Failed operations remove staging.
func Backup(ctx context.Context, store *state.Store, artifactRoot, targetDir string) error {
	if store == nil {
		return errors.New("archive: nil store")
	}
	return publish(ctx, targetDir, func(stage string, dst *os.Root) error {
		if err := store.Backup(ctx, filepath.Join(stage, "database.db")); err != nil {
			return err
		}
		refs, err := snapshotReferences(ctx, filepath.Join(stage, "database.db"))
		if err != nil {
			return err
		}
		src, err := openRoot(artifactRoot)
		if err != nil {
			return fmt.Errorf("archive: artifact root: %w", err)
		}
		defer src.Close()
		required, err := dependencies(ctx, src, refs)
		if err != nil {
			return err
		}
		files := make([]File, 0, len(required)+1)
		dbFile, err := describe(ctx, dst, "database.db", "database")
		if err != nil {
			return err
		}
		files = append(files, dbFile)
		for _, p := range sortedKeys(required) {
			f, err := copyFile(ctx, src, p, dst, "artifacts/"+p, required[p].Kind)
			if err != nil {
				return err
			}
			if f.SHA256 != required[p].SHA256 {
				return fmt.Errorf("archive: changed or corrupt artifact %s", p)
			}
			files = append(files, f)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		m := Manifest{Format: FormatVersion, Database: "database.db", ArtifactRoot: "artifacts", Files: files}
		if err := validateContents(ctx, stage, dst, m); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		return writeBytes(dst, "manifest.json", append(raw, '\n'))
	})
}

// Restore copies only declared files into private staging, checks file hashes,
// SQLite integrity/foreign keys and the snapshot's complete document closure,
// then publishes an isolated, previously nonexistent directory. It does not
// migrate schemas, recover leases, or start a worker.
func Restore(ctx context.Context, backupDir, targetDir string) error {
	src, err := openRoot(backupDir)
	if err != nil {
		return err
	}
	defer src.Close()
	raw, err := readBytes(src, "manifest.json", manifestLimit)
	if err != nil {
		return err
	}
	var m Manifest
	if err := decodeStrict(raw, &m); err != nil {
		return fmt.Errorf("archive: manifest: %w", err)
	}
	if err := validateManifest(m); err != nil {
		return err
	}
	return publish(ctx, targetDir, func(stage string, dst *os.Root) error {
		for _, f := range m.Files {
			got, err := copyFile(ctx, src, f.Path, dst, f.Path, f.Kind)
			if err != nil {
				return err
			}
			if got.SHA256 != f.SHA256 || got.Size != f.Size {
				return fmt.Errorf("archive: file integrity failed: %s", f.Path)
			}
		}
		if err := validateContents(ctx, stage, dst, m); err != nil {
			return err
		}
		return writeBytes(dst, "manifest.json", raw)
	})
}

func validHash(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func safePath(p string) bool {
	return p != "" && p != "." && !filepath.IsAbs(p) && !strings.ContainsAny(p, "\\\x00:") && filepath.ToSlash(p) == p && filepath.Clean(p) == p && !strings.HasPrefix(p, "../")
}
func validateManifest(m Manifest) error {
	if m.Format != FormatVersion || m.Database != "database.db" || m.ArtifactRoot != "artifacts" {
		return errors.New("archive: unsupported manifest layout or format")
	}
	last := ""
	databases := 0
	for _, f := range m.Files {
		if !safePath(f.Path) || f.Path <= last || !validHash(f.SHA256) || f.Size < 0 {
			return errors.New("archive: invalid or duplicate manifest file")
		}
		last = f.Path
		if f.Path == "database.db" && f.Kind == "database" {
			databases++
			continue
		}
		if !strings.HasPrefix(f.Path, "artifacts/") || (f.Kind != "artifact" && f.Kind != "document-manifest" && f.Kind != "document-identity") {
			return fmt.Errorf("archive: unsupported file %s", f.Path)
		}
	}
	if databases != 1 {
		return errors.New("archive: missing database")
	}
	return nil
}

func openSnapshot(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"query_only(1)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
func snapshotReferences(ctx context.Context, path string) (references, error) {
	r := references{artifacts: map[string]string{}, documents: map[string][]library.Document{}}
	db, err := openSnapshot(path)
	if err != nil {
		return r, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var result string
		if err = rows.Scan(&result); err != nil {
			break
		}
		if result != "ok" {
			err = fmt.Errorf("archive: SQLite integrity_check: %s", result)
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return r, err
	}
	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return r, err
	}
	hasViolation := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, err
	}
	if hasViolation {
		return r, errors.New("archive: SQLite foreign_key_check failed")
	}
	// These are fixed table names from state/migrations.go; no Store.Open or migration.
	tables := []string{"library_versions", "library_batches", "library_outputs"}
	// Older snapshots predate undated source observations. Enumerate this table
	// when present without changing either snapshot schema or migration history.
	var observations int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='library_source_observations')`).Scan(&observations); err != nil {
		return r, err
	}
	if observations != 0 {
		tables = append(tables, "library_source_observations")
	}
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, "SELECT data FROM "+table)
		if err != nil {
			return r, fmt.Errorf("archive: read %s: %w", table, err)
		}
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				break
			}
			var value any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err = dec.Decode(&value); err != nil {
				break
			}
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = errors.New("archive: snapshot JSON trailing data")
				break
			}
			if err = collect(value, &r); err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return r, fmt.Errorf("archive: %s data: %w", table, err)
		}
	}
	return r, nil
}
func collect(v any, r *references) error {
	switch x := v.(type) {
	case []any:
		for _, y := range x {
			if err := collect(y, r); err != nil {
				return err
			}
		}
	case map[string]any:
		p, hasPath := x["path"]
		h, hasHash := x["sha256"]
		// Page/block hashes have no path. Every path+sha256 pair is an Artifact,
		// including new nested output contracts and historic generations.
		if hasPath {
			path, ok := p.(string)
			hash, hok := h.(string)
			if !hasHash || !ok || !hok || !safePath(path) || !validHash(hash) {
				return errors.New("archive: invalid snapshot artifact path/hash")
			}
			if old, ok := r.artifacts[path]; ok && old != hash {
				return fmt.Errorf("archive: conflicting hashes for %s", path)
			}
			r.artifacts[path] = hash
			if strings.HasPrefix(path, "documents/") {
				parts := strings.Split(path, "/")
				if len(parts) != 3 || !validHash(parts[1]) {
					return errors.New("archive: invalid document artifact path")
				}
				if _, ok := r.documents[parts[1]]; !ok {
					r.documents[parts[1]] = nil
				}
			}
		}
		if _, ok := x["source_file"]; ok {
			if _, ok := x["text_file"]; ok {
				raw, err := json.Marshal(x)
				if err != nil {
					return err
				}
				var d library.Document
				if err = json.Unmarshal(raw, &d); err != nil {
					return err
				}
				if !validHash(d.ID) {
					return errors.New("archive: invalid snapshot document ID")
				}
				r.documents[d.ID] = append(r.documents[d.ID], d)
			}
		}
		for _, y := range x {
			if err := collect(y, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// Only manifests of snapshot-referenced documents are retained. Their two files
// must both occur in the snapshot with the same hashes; identity pointers must
// point to that exact immutable document. Unreferenced/live documents are ignored.
func dependencies(ctx context.Context, root *os.Root, r references) (map[string]File, error) {
	want := map[string]File{}
	for p, h := range r.artifacts {
		want[p] = File{Path: p, SHA256: h, Kind: "artifact"}
	}
	repo := document.Repository{Root: root.Name(), MaxBytes: 1 << 30}
	for _, id := range sortedKeys(r.documents) {
		p := "documents/" + id + "/manifest.json"
		raw, err := readBytes(root, p, 256<<20)
		if err != nil {
			return nil, fmt.Errorf("archive: document manifest %s: %w", id, err)
		}
		d, err := repo.Load(ctx, id)
		if err != nil && !errors.Is(err, document.ErrQuality) {
			return nil, fmt.Errorf("archive: document %s: %w", id, err)
		}
		for _, expected := range r.documents[id] {
			if _, err := repo.Load(ctx, expected); err != nil && !errors.Is(err, document.ErrQuality) {
				return nil, fmt.Errorf("archive: snapshot document %s: %w", id, err)
			}
		}
		for _, a := range []library.Artifact{d.Source, d.Text} {
			if r.artifacts[a.Path] != a.SHA256 {
				return nil, fmt.Errorf("archive: document %s is not consistent with snapshot artifacts", id)
			}
		}
		if _, exists := want[p]; exists {
			return nil, errors.New("archive: artifact collides with document manifest")
		}
		want[p] = File{Path: p, SHA256: hashBytes(raw), Kind: "document-manifest"}
		index := "identities/" + hashBytes([]byte(d.Identity.Key())) + ".json"
		pointer, err := readBytes(root, index, 128)
		if err != nil {
			return nil, fmt.Errorf("archive: identity pointer: %w", err)
		}
		if string(pointer) != id {
			return nil, fmt.Errorf("archive: identity pointer disagrees with snapshot document %s", id)
		}
		f := File{Path: index, SHA256: hashBytes(pointer), Kind: "document-identity"}
		if old, ok := want[index]; ok && old.SHA256 != f.SHA256 {
			return nil, errors.New("archive: conflicting document identities")
		}
		want[index] = f
	}
	return want, nil
}
func validateContents(ctx context.Context, stage string, root *os.Root, m Manifest) error {
	if err := validateManifest(m); err != nil {
		return err
	}
	for _, f := range m.Files {
		got, err := describe(ctx, root, f.Path, f.Kind)
		if err != nil {
			return err
		}
		if got.SHA256 != f.SHA256 || got.Size != f.Size {
			return fmt.Errorf("archive: file integrity failed: %s", f.Path)
		}
	}
	r, err := snapshotReferences(ctx, filepath.Join(stage, "database.db"))
	if err != nil {
		return err
	}
	if err := root.MkdirAll("artifacts", 0700); err != nil {
		return err
	}
	artifacts, err := root.OpenRoot("artifacts")
	if err != nil {
		return err
	}
	defer artifacts.Close()
	want, err := dependencies(ctx, artifacts, r)
	if err != nil {
		return err
	}
	if len(m.Files) != len(want)+1 {
		return errors.New("archive: manifest differs from snapshot dependency closure")
	}
	for _, f := range m.Files {
		if f.Path == "database.db" {
			continue
		}
		expected, ok := want[strings.TrimPrefix(f.Path, "artifacts/")]
		if !ok || f.Kind != expected.Kind || f.SHA256 != expected.SHA256 {
			return fmt.Errorf("archive: manifest dependency mismatch: %s", f.Path)
		}
	}
	return ctx.Err()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func openRoot(path string) (*os.Root, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("archive: empty root")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive: root is not a real directory")
	}
	return os.OpenRoot(abs)
}
func noSymlink(root *os.Root, path string) error {
	if !safePath(path) {
		return errors.New("archive: unsafe path")
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		fi, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !fi.IsDir()) {
			return fmt.Errorf("archive: unsafe file component in %s", path)
		}
	}
	return nil
}
func openRegular(root *os.Root, path string) (*os.File, error) {
	if err := noSymlink(root, path); err != nil {
		return nil, err
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = errors.New("archive: not a regular file")
		}
		return nil, err
	}
	return f, nil
}
func readBytes(root *os.Root, path string, max int64) ([]byte, error) {
	f, err := openRegular(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		return nil, errors.New("archive: oversized metadata")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if int64(len(b)) > max {
		return nil, errors.New("archive: oversized metadata")
	}
	return b, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func describe(ctx context.Context, root *os.Root, path, kind string) (File, error) {
	f, err := openRegular(root, path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, f})
	if err != nil {
		return File{}, err
	}
	return File{Path: path, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Kind: kind}, nil
}
func createFile(root *os.Root, path string) (*os.File, error) {
	if !safePath(path) {
		return nil, errors.New("archive: unsafe output path")
	}
	dir := filepath.ToSlash(filepath.Dir(path))
	if dir != "." {
		if err := root.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if err := noSymlink(root, dir); err != nil {
			return nil, err
		}
	}
	return root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}
func copyFile(ctx context.Context, src *os.Root, source string, dst *os.Root, path, kind string) (File, error) {
	in, err := openRegular(src, source)
	if err != nil {
		return File{}, fmt.Errorf("archive: read %s: %w", source, err)
	}
	defer in.Close()
	out, err := createFile(dst, path)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), contextReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return File{}, err
	}
	return File{Path: path, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Kind: kind}, nil
}
func writeBytes(root *os.Root, path string, b []byte) error {
	f, err := createFile(root, path)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func publish(ctx context.Context, target string, build func(string, *os.Root) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(target) == "" {
		return errors.New("archive: empty target directory")
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	parent, err := openRoot(filepath.Dir(abs))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(abs)
	if !safePath(name) {
		return errors.New("archive: invalid target directory")
	}
	if _, err := parent.Lstat(name); err == nil {
		return fmt.Errorf("archive: target already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Staging is a private sibling of the target on the same filesystem, so a
	// target remains absent until every dependency and the manifest is verified.
	stage, err := os.MkdirTemp(parent.Name(), ".archive-")
	if err != nil {
		return err
	}
	stageName := filepath.Base(stage)
	defer parent.RemoveAll(stageName)
	root, err := parent.OpenRoot(stageName)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = build(stage, root); err != nil {
		return err
	}
	if err = syncTree(root); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	fd, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer fd.Close()
	if err = renameExclusive(fd, stageName, name); err != nil {
		return fmt.Errorf("archive: publish: %w", err)
	}
	return fd.Sync()
}
func syncTree(root *os.Root) error {
	return filepath.WalkDir(root.Name(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root.Name(), path)
		if err != nil {
			return err
		}
		f, err := root.Open(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		return err
	})
}
