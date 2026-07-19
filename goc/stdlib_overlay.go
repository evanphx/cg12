package goc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const stdlibOverlayEnvironment = "GOC_STDLIB_OVERLAY"

type stdlibOverlayManifest struct {
	Version int                  `json:"version"`
	Files   []stdlibOverlayEntry `json:"files"`
}

type stdlibOverlayEntry struct {
	Package        string   `json:"package"`
	Path           string   `json:"path"`
	File           string   `json:"file,omitempty"`
	Mode           string   `json:"mode"`
	Reason         string   `json:"reason"`
	UpstreamSHA256 string   `json:"upstream_sha256,omitempty"`
	GOOS           []string `json:"goos,omitempty"`
	GOARCH         []string `json:"goarch,omitempty"`
	RemoveWhen     string   `json:"remove_when,omitempty"`
	CallConvention string   `json:"call_convention,omitempty"`
	ManagedFrame   *bool    `json:"managed_frame,omitempty"`
	NoSplit        bool     `json:"nosplit,omitempty"`

	origin string
}

type stdlibOverlay struct {
	root      string
	manifest  string
	files     map[string]stdlibOverlayEntry
	additions map[string][]stdlibOverlayEntry
}

type sourceOverlayUse struct {
	Path       string
	File       string
	Mode       string
	Reason     string
	RemoveWhen string
}

func loadStdlibOverlay(root string) (*stdlibOverlay, error) {
	selection := strings.TrimSpace(os.Getenv(stdlibOverlayEnvironment))
	if selection == "off" || selection == "none" || selection == "0" {
		return nil, nil
	}

	manifestPath := selection
	if manifestPath == "" || manifestPath == "default" {
		manifestPath = filepath.Join(root, "overlays", "manifest.json")
	}
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("standard library overlay manifest: %w", err)
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("standard library overlay manifest %s: %w", manifestPath, err)
	}
	var manifest stdlibOverlayManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("standard library overlay manifest %s: %w", manifestPath, err)
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("standard library overlay manifest %s has version %d, want 1", manifestPath, manifest.Version)
	}

	overlay := &stdlibOverlay{
		root:      root,
		manifest:  manifestPath,
		files:     make(map[string]stdlibOverlayEntry),
		additions: make(map[string][]stdlibOverlayEntry),
	}
	manifestDirectory := filepath.Dir(manifestPath)
	for index := range manifest.Files {
		entry := manifest.Files[index]
		if !overlayTargetMatches(entry.GOOS, runtime.GOOS) || !overlayTargetMatches(entry.GOARCH, runtime.GOARCH) {
			continue
		}
		entry.Path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path)))
		if entry.Path == "." || strings.HasPrefix(entry.Path, "../") || !strings.HasPrefix(entry.Path, "src/") {
			return nil, fmt.Errorf("standard library overlay path %q must be beneath src", entry.Path)
		}
		if entry.Package == "" {
			return nil, fmt.Errorf("standard library overlay %s has no package", entry.Path)
		}
		packageDirectory := filepath.ToSlash(filepath.Join("src", filepath.FromSlash(entry.Package))) + "/"
		if !strings.HasPrefix(entry.Path, packageDirectory) {
			return nil, fmt.Errorf("standard library overlay %s is outside package %s", entry.Path, entry.Package)
		}
		if entry.Reason == "" {
			return nil, fmt.Errorf("standard library overlay %s has no reason", entry.Path)
		}
		switch entry.Mode {
		case "replace", "add":
			if entry.File == "" {
				return nil, fmt.Errorf("standard library overlay %s has no overlay file", entry.Path)
			}
			entry.origin, err = overlayPathWithin(manifestDirectory, entry.File)
			if err != nil {
				return nil, fmt.Errorf("standard library overlay %s: %w", entry.Path, err)
			}
			if _, err := os.Stat(entry.origin); err != nil {
				return nil, fmt.Errorf("standard library overlay %s: %w", entry.Path, err)
			}
			extension := strings.ToLower(filepath.Ext(entry.origin))
			if extension != ".go" && extension != ".ssa" {
				return nil, fmt.Errorf("standard library overlay %s uses %s; overlays must be Go or native .ssa", entry.Path, extension)
			}
			logicalExtension := strings.ToLower(filepath.Ext(entry.Path))
			if entry.Mode == "replace" && (logicalExtension != ".go" || extension != ".go") {
				return nil, fmt.Errorf("standard library overlay replacement %s must replace Go with Go", entry.Path)
			}
			if entry.Mode == "add" && logicalExtension != extension {
				return nil, fmt.Errorf("standard library overlay addition %s must have the same extension as %s", entry.Path, entry.origin)
			}
		case "delete":
			if entry.File != "" {
				return nil, fmt.Errorf("deleted standard library file %s must not name an overlay file", entry.Path)
			}
		default:
			return nil, fmt.Errorf("standard library overlay %s has invalid mode %q", entry.Path, entry.Mode)
		}

		upstreamPath := filepath.Join(root, filepath.FromSlash(entry.Path))
		if entry.Mode == "add" {
			if _, err := os.Stat(upstreamPath); err == nil {
				return nil, fmt.Errorf("added standard library overlay %s already exists upstream", entry.Path)
			}
			overlay.additions[entry.Package] = append(overlay.additions[entry.Package], entry)
			continue
		}
		if entry.UpstreamSHA256 == "" {
			return nil, fmt.Errorf("standard library overlay %s has no upstream SHA-256", entry.Path)
		}
		if err := verifyOverlayUpstream(upstreamPath, entry.UpstreamSHA256); err != nil {
			return nil, fmt.Errorf("standard library overlay %s: %w", entry.Path, err)
		}
		if _, exists := overlay.files[entry.Path]; exists {
			return nil, fmt.Errorf("standard library overlay %s is declared more than once", entry.Path)
		}
		overlay.files[entry.Path] = entry
	}
	return overlay, nil
}

func overlayTargetMatches(targets []string, current string) bool {
	if len(targets) == 0 {
		return true
	}
	for _, target := range targets {
		if target == current {
			return true
		}
	}
	return false
}

func overlayPathWithin(root, name string) (string, error) {
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(name)))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("overlay file %q escapes the manifest directory", name)
	}
	return path, nil
}

func verifyOverlayUpstream(path, wantHash string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read upstream file: %w", err)
	}
	if wantHash == "" {
		return nil
	}
	digest := sha256.Sum256(content)
	gotHash := hex.EncodeToString(digest[:])
	if gotHash != strings.ToLower(wantHash) {
		return fmt.Errorf("upstream hash is %s, manifest expects %s", gotHash, wantHash)
	}
	return nil
}

func (overlay *stdlibOverlay) resolve(path string) (origin string, content []byte, entry *stdlibOverlayEntry, deleted bool, err error) {
	if overlay == nil {
		content, err = os.ReadFile(path)
		return path, content, nil, false, err
	}
	relative, relativeErr := filepath.Rel(overlay.root, path)
	if relativeErr != nil {
		content, err = os.ReadFile(path)
		return path, content, nil, false, err
	}
	logicalPath := filepath.ToSlash(relative)
	selected, found := overlay.files[logicalPath]
	if !found {
		content, err = os.ReadFile(path)
		return path, content, nil, false, err
	}
	if selected.Mode == "delete" {
		return path, nil, &selected, true, nil
	}
	content, err = os.ReadFile(selected.origin)
	return selected.origin, content, &selected, false, err
}

func overlayUse(entry *stdlibOverlayEntry) sourceOverlayUse {
	if entry == nil {
		return sourceOverlayUse{}
	}
	return sourceOverlayUse{
		Path:       entry.Path,
		File:       entry.origin,
		Mode:       entry.Mode,
		Reason:     entry.Reason,
		RemoveWhen: entry.RemoveWhen,
	}
}
