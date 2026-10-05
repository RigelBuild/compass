package appconfig

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

const saveFileMode = 0o600

// The link commits the save, so directory sync errors are reported after it.
var syncDirectory = syncDir

// SaveClient validates and writes a client config without replacing app.toml.
func SaveClient(path string, cfg Config, caPEM []byte) (_ Config, retErr error) {
	if cfg.Mode != ModeClient {
		return Config{}, fmt.Errorf("appconfig: saving client config requires mode=client, got %s", cfg.Mode)
	}
	serverURL, err := NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return Config{}, err
	}
	if err := ensureConfigDir(filepath.Dir(path)); err != nil {
		return Config{}, err
	}
	cfg = Config{Mode: ModeClient, ServerURL: serverURL}
	if len(caPEM) > 0 {
		cfg.CACert, err = saveCACert(filepath.Dir(path), caPEM)
		if err != nil {
			return Config{}, err
		}
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, removeFile(cfg.CACert))
			}
		}()
	}
	return saveConfig(path, cfg)
}

// SaveEmbedded writes embedded mode without replacing an existing app.toml.
func SaveEmbedded(path string) error {
	_, err := saveConfig(path, Config{Mode: ModeEmbedded})
	return err
}
func saveConfig(path string, cfg Config) (_ Config, retErr error) {
	dir := filepath.Dir(path)
	if err := ensureConfigDir(dir); err != nil {
		return Config{}, err
	}
	if _, err := os.Lstat(path); err == nil {
		return Config{}, ErrConfigExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("appconfig: checking config path %q: %w", path, err)
	}

	data, err := encodeConfig(cfg)
	if err != nil {
		return Config{}, err
	}
	saved, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("appconfig: validating saved config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".app.toml-*.tmp")
	if err != nil {
		return Config{}, fmt.Errorf("appconfig: creating config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := removeFile(tmpPath); err != nil {
			slog.Error("appconfig: cleaning up config temp file", "path", tmpPath, "error", err)
		}
	}()
	if err := writeSyncedFile(tmp, data); err != nil {
		return Config{}, errors.Join(
			fmt.Errorf("appconfig: writing config temp file: %w", err),
			removeFile(tmpPath),
		)
	}
	if err := os.Link(tmpPath, path); err != nil {
		removeErr := removeFile(tmpPath)
		if errors.Is(err, os.ErrExist) {
			return Config{}, errors.Join(ErrConfigExists, removeErr)
		}
		return Config{}, errors.Join(fmt.Errorf("appconfig: linking config into place: %w", err), removeErr)
	}
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("appconfig: removing linked config temp file", "path", tmpPath, "error", err)
	}
	if err := syncDirectory(dir); err != nil {
		slog.Error("appconfig: syncing config directory after save", "path", dir, "error", err)
	}
	return saved, nil
}

func ensureConfigDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("appconfig: creating config directory %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: config directory mode must be 0700; it contains private CA bytes and app.toml
		return fmt.Errorf("appconfig: setting config directory mode: %w", err)
	}
	return nil
}

func encodeConfig(cfg Config) ([]byte, error) {
	fc := struct {
		Mode      string `toml:"mode"`
		ServerURL string `toml:"server_url,omitempty"`
		CACert    string `toml:"ca_cert,omitempty"`
	}{
		Mode:      cfg.Mode.String(),
		ServerURL: cfg.ServerURL,
		CACert:    cfg.CACert,
	}
	var encoded bytes.Buffer
	if _, err := encoded.WriteString("# Written by Compass. Manual edits are supported.\n"); err != nil {
		return nil, fmt.Errorf("appconfig: writing config header: %w", err)
	}
	if err := toml.NewEncoder(&encoded).Encode(fc); err != nil {
		return nil, fmt.Errorf("appconfig: encoding app.toml: %w", err)
	}
	return encoded.Bytes(), nil
}

func saveCACert(dir string, caPEM []byte) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("appconfig: generating CA filename: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("server-ca-%x.pem", random[:]))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, saveFileMode) //nolint:gosec // G304: path is generated beside the caller-resolved config path
	if err != nil {
		return "", fmt.Errorf("appconfig: creating CA copy: %w", err)
	}
	if err := writeSyncedFile(file, caPEM); err != nil {
		return "", errors.Join(fmt.Errorf("appconfig: writing CA copy: %w", err), removeFile(path))
	}
	return path, nil
}

func writeSyncedFile(file *os.File, data []byte) error {
	if err := file.Chmod(saveFileMode); err != nil {
		return errors.Join(fmt.Errorf("setting file mode: %w", err), file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(fmt.Errorf("writing file: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("syncing file: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	return nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %q: %w", path, err)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: path is the directory of the caller-resolved config path
	if err != nil {
		return fmt.Errorf("opening directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return errors.Join(fmt.Errorf("syncing directory: %w", err), dir.Close())
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("closing directory: %w", err)
	}
	return nil
}
