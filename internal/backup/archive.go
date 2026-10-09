// Package backup implements portable, checked archives without host operations.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"abr/internal/config"
	"abr/internal/manager"
	"abr/internal/ports"
)

const manifestName = "manifest.json"
const maxManifest = 8 << 20

type App struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Enabled    bool   `json:"enabled"`
}

type Settings struct {
	Hostname          string `json:"hostname"`
	RoadRunnerVersion string `json:"roadrunner_version"`
	NoFirewall        bool   `json:"no_firewall"`
	NoRedis           bool   `json:"no_redis"`
	NoImages          bool   `json:"no_images"`
}

type File struct {
	Size      int64  `json:"size"`
	Mode      int64  `json:"mode"`
	Directory bool   `json:"directory,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type Manifest struct {
	Version   int             `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	Config    config.Config   `json:"config"`
	Ports     ports.Registry  `json:"ports"`
	Apps      []App           `json:"apps"`
	Settings  Settings        `json:"settings"`
	Files     map[string]File `json:"files"`
}

func safeName(name string) bool {
	return name != "." && fs.ValidPath(name) && path.Clean(name) == name && !strings.ContainsAny(name, "\\\x00\r\n")
}

func (m Manifest) Validate() error {
	if m.Version != 1 || m.CreatedAt.IsZero() || m.Files == nil {
		return fmt.Errorf("invalid backup manifest")
	}
	if err := m.Config.Validate(); err != nil {
		return err
	}
	if err := m.Ports.Validate(); err != nil {
		return err
	}
	if err := manager.CheckReservations(m.Config, m.Ports); err != nil {
		return err
	}
	if err := config.ValidateHostname(m.Settings.Hostname); err != nil {
		return err
	}
	if len(m.Apps) != len(m.Config.Apps) {
		return fmt.Errorf("backup app selection differs from config")
	}
	seen := map[string]bool{}
	for _, a := range m.Apps {
		if seen[a.Name] || a.Repository == "" || a.Branch == "" || strings.ContainsAny(a.Repository+a.Branch, "\x00\r\n") {
			return fmt.Errorf("invalid backup repository record")
		}
		exists := false
		for _, c := range m.Config.Apps {
			if c.Name == a.Name {
				exists = true
			}
		}
		if !exists {
			return fmt.Errorf("unknown backup app")
		}
		seen[a.Name] = true
	}
	for name, f := range m.Files {
		if !safeName(name) || name == manifestName || f.Size < 0 || f.Mode < 0 || f.Mode > 0777 {
			return fmt.Errorf("invalid backup file record")
		}
		if f.Directory {
			if f.Size != 0 || f.SHA256 != "" {
				return fmt.Errorf("invalid directory record")
			}
		} else {
			sum, err := hex.DecodeString(f.SHA256)
			if err != nil || len(sum) != sha256.Size {
				return fmt.Errorf("invalid backup checksum")
			}
		}
	}
	return nil
}

// Write streams a private staging tree. Symlinks and special files are rejected.
func Write(w io.Writer, directory string, manifest *Manifest) (result error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() {
		closeTar := tw.Close()
		closeGzip := gz.Close()
		if result == nil {
			if closeTar != nil {
				result = closeTar
			} else {
				result = closeGzip
			}
		}
	}()
	manifest.Files = map[string]File{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if !safeName(name) || name == manifestName {
			return fmt.Errorf("invalid archive path")
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		record := File{Mode: int64(info.Mode().Perm()), Directory: info.IsDir()}
		header := &tar.Header{Name: name, Mode: record.Mode, Typeflag: tar.TypeDir}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("backup does not support symlinks or special files: %s", name)
			}
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			current, err := f.Stat()
			if err != nil {
				return err
			}
			if !current.Mode().IsRegular() {
				return fmt.Errorf("backup file changed type")
			}
			record.Size = current.Size()
			header.Typeflag, header.Size = tar.TypeReg, record.Size
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			sum := sha256.New()
			if _, err := io.CopyN(io.MultiWriter(tw, sum), f, record.Size); err != nil {
				return err
			}
			var extra [1]byte
			if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
				return fmt.Errorf("backup file changed while reading")
			}
			record.SHA256 = hex.EncodeToString(sum.Sum(nil))
		} else if err := tw.WriteHeader(header); err != nil {
			return err
		}
		manifest.Files[name] = record
		return nil
	})
	if err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(data) > maxManifest {
		return fmt.Errorf("backup manifest too large")
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

// Extract writes only into a caller-created private staging directory. No archive
// path is ever interpreted as a host destination. Checksums are verified before use.
func Extract(r io.Reader, directory string) (Manifest, error) {
	var manifest Manifest
	root, err := os.OpenRoot(directory)
	if err != nil {
		return manifest, err
	}
	defer root.Close()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return manifest, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	records := map[string]File{}
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !safeName(name) || header.Size < 0 || header.Mode < 0 || header.Mode > 0777 || found {
			return manifest, fmt.Errorf("unsafe backup entry")
		}
		if name == manifestName {
			if header.Typeflag != tar.TypeReg || header.Size > maxManifest {
				return manifest, fmt.Errorf("invalid backup manifest entry")
			}
			decoder := json.NewDecoder(io.LimitReader(tr, maxManifest+1))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&manifest); err != nil {
				return manifest, err
			}
			if decoder.Decode(new(any)) != io.EOF {
				return manifest, fmt.Errorf("trailing backup manifest data")
			}
			found = true
			continue
		}
		if _, exists := records[name]; exists {
			return manifest, fmt.Errorf("duplicate backup entry")
		}
		if len(records) >= 1000000 {
			return manifest, fmt.Errorf("too many backup entries")
		}
		record := File{Size: header.Size, Mode: header.Mode}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return manifest, fmt.Errorf("invalid backup directory")
			}
			if err := root.MkdirAll(name, 0700); err != nil {
				return manifest, err
			}
			record.Directory = true
		} else if header.Typeflag == tar.TypeReg {
			if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
				return manifest, err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return manifest, err
			}
			sum := sha256.New()
			_, copyErr := io.CopyN(io.MultiWriter(f, sum), tr, header.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return manifest, copyErr
			}
			if closeErr != nil {
				return manifest, closeErr
			}
			record.SHA256 = hex.EncodeToString(sum.Sum(nil))
		} else {
			return manifest, fmt.Errorf("backup links and special files are forbidden")
		}
		records[name] = record
	}
	// Consume the gzip footer so truncated/corrupt gzip streams cannot pass checks.
	if n, err := io.Copy(io.Discard, gz); err != nil || n != 0 {
		if err != nil {
			return manifest, err
		}
		return manifest, fmt.Errorf("trailing archive data")
	}
	if !found {
		return manifest, fmt.Errorf("backup manifest missing")
	}
	if err := manifest.Validate(); err != nil {
		return manifest, err
	}
	if len(records) != len(manifest.Files) {
		return manifest, fmt.Errorf("backup file inventory mismatch")
	}
	for name, record := range records {
		if expected, ok := manifest.Files[name]; !ok || expected != record {
			return manifest, fmt.Errorf("backup checksum or metadata mismatch: %s", name)
		}
	}
	return manifest, nil
}

// CopyTree copies scoped data through bounded roots, rejecting links and devices.
// Parents remain private; caller applies host ownership/ACLs after installation.
func CopyTree(source, destination string) error {
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	targetRoot, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer targetRoot.Close()
	return fs.WalkDir(sourceRoot.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := sourceRoot.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return targetRoot.MkdirAll(name, 0700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("persistent data must not contain links or special files: %s", name)
		}
		f, err := sourceRoot.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		current, err := f.Stat()
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() {
			return fmt.Errorf("persistent file changed type")
		}
		if err := targetRoot.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		out, err := targetRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, f)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
