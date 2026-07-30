package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	DefaultProfile = "default"
	ProfileEnvVar  = "AGENTMODEL_PROFILE"

	anthropicProfileEnvVar = "ANTHROPIC_OAUTH_TOKEN_DIR"
	anthropicProfileSubdir = "anthropic"

	chatgptProfileEnvVar = "CHATGPT_TOKEN_DIR"
	chatgptProfileSubdir = "chatgpt"

	activeProfileFileName = "profile"
)

// profileStore binds the provider-agnostic profile machinery to a single
// provider's token-dir root (the same envVar/subdir pair NewChatGPTOAuth and
// the Anthropic refreshable use). Each provider keeps its own profiles and its
// own active-profile sidecar, so `--profile work` for ChatGPT and Anthropic
// are independent stores under separate base directories.
type profileStore struct {
	envVar string
	subdir string
}

var (
	anthropicStore = profileStore{envVar: anthropicProfileEnvVar, subdir: anthropicProfileSubdir}
	chatgptStore   = profileStore{envVar: chatgptProfileEnvVar, subdir: chatgptProfileSubdir}
)

// resolveTokenDir returns the auth token directory for a profile. tokenDirFlag
// is an absolute override and preserves legacy "directory containing
// auth.json" behavior, in which case the resolved profile name is empty.
func (s profileStore) resolveTokenDir(profileFlag, tokenDirFlag string) (tokenDir, profile string, err error) {
	if strings.TrimSpace(tokenDirFlag) != "" {
		return tokenDirFlag, "", nil
	}
	profile, err = s.resolveProfile(profileFlag)
	if err != nil {
		return "", "", err
	}
	return tokenDirForProfile(s.envVar, s.subdir, profile), profile, nil
}

// resolveProfile resolves the active profile name using
// flag > AGENTMODEL_PROFILE > sidecar > default.
func (s profileStore) resolveProfile(profileFlag string) (string, error) {
	name := strings.TrimSpace(profileFlag)
	if name == "" {
		name = strings.TrimSpace(os.Getenv(ProfileEnvVar))
	}
	if name == "" {
		active, err := s.readActive()
		if err != nil {
			return "", err
		}
		name = active
	}
	if name == "" {
		name = DefaultProfile
	}
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	return name, nil
}

func (s profileStore) baseDir() string {
	return defaultTokenDir(s.envVar, s.subdir)
}

// readActive returns the sidecar-selected profile. Missing sidecar is not an
// error and returns "".
func (s profileStore) readActive() (string, error) {
	path := filepath.Join(s.baseDir(), activeProfileFileName)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read active profile: %w", err)
	}
	name := strings.TrimSpace(string(b))
	if name == "" {
		return "", nil
	}
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	return name, nil
}

// writeActive stores the active profile in the sidecar file.
func (s profileStore) writeActive(name string) error {
	if err := ValidateProfileName(name); err != nil {
		return err
	}
	base := s.baseDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return fmt.Errorf("mkdir profile base: %w", err)
	}
	// 0o600: the sidecar name is not a secret like a token, but profile
	// names can reveal sensitive context (customer codenames, "prod" etc.)
	// to other local users — match the policy we use for auth.json.
	return atomicWriteFileMode(filepath.Join(base, activeProfileFileName), []byte(name+"\n"), 0o600)
}

// clearActive removes the active profile sidecar. Missing sidecar is not an
// error.
func (s profileStore) clearActive() error {
	path := filepath.Join(s.baseDir(), activeProfileFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear active profile: %w", err)
	}
	return nil
}

var (
	// Lowercase-only so two names can't alias to the same directory on
	// case-insensitive filesystems (default macOS APFS, Windows NTFS).
	// Without this constraint, `--profile Work` followed by `--profile
	// work` would silently overwrite the first profile's tokens.
	profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reservedNames = map[string]struct{}{
		"profile":   {},
		"auth.json": {},
		"tmp":       {},
		".lock":     {},
		"type":      {},
		"access":    {},
		"refresh":   {},
		"expires":   {},
		"con":       {},
		"prn":       {},
		"aux":       {},
		"nul":       {},
		"com1":      {},
		"com2":      {},
		"com3":      {},
		"com4":      {},
		"com5":      {},
		"com6":      {},
		"com7":      {},
		"com8":      {},
		"com9":      {},
		"lpt1":      {},
		"lpt2":      {},
		"lpt3":      {},
		"lpt4":      {},
		"lpt5":      {},
		"lpt6":      {},
		"lpt7":      {},
		"lpt8":      {},
		"lpt9":      {},
	}
)

// ResolveProfileTokenDir returns the auth token directory for an Anthropic
// profile. tokenDirFlag is an absolute override and preserves legacy
// "directory containing auth.json" behavior.
func ResolveProfileTokenDir(profileFlag, tokenDirFlag string) (tokenDir, profile string, err error) {
	return anthropicStore.resolveTokenDir(profileFlag, tokenDirFlag)
}

// ResolveChatGPTProfileTokenDir returns the auth token directory for a ChatGPT
// profile, mirroring ResolveProfileTokenDir for the ChatGPT token-dir root.
func ResolveChatGPTProfileTokenDir(profileFlag, tokenDirFlag string) (tokenDir, profile string, err error) {
	return chatgptStore.resolveTokenDir(profileFlag, tokenDirFlag)
}

// ResolveAnthropicProfile resolves the active Anthropic profile name using
// flag > AGENTMODEL_PROFILE > sidecar > default.
func ResolveAnthropicProfile(profileFlag string) (string, error) {
	return anthropicStore.resolveProfile(profileFlag)
}

// ReadActiveProfile returns the sidecar-selected Anthropic profile. Missing
// sidecar is not an error and returns "".
func ReadActiveProfile() (string, error) {
	return anthropicStore.readActive()
}

// WriteActiveProfile stores the active Anthropic profile in the sidecar file.
func WriteActiveProfile(name string) error {
	return anthropicStore.writeActive(name)
}

// WriteActiveChatGPTProfile stores the active ChatGPT profile in the sidecar
// file under the ChatGPT token-dir root.
func WriteActiveChatGPTProfile(name string) error {
	return chatgptStore.writeActive(name)
}

// ClearActiveProfile removes the active Anthropic profile sidecar. Missing
// sidecar is not an error.
func ClearActiveProfile() error {
	return anthropicStore.clearActive()
}

// ListProfiles returns profiles that have an auth.json file.
func ListProfiles() ([]string, error) {
	base := anthropicStore.baseDir()
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if err := ValidateProfileName(name); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, name, "auth.json")); err == nil {
			out = append(out, name)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "auth.json")); err == nil && !slices.Contains(out, DefaultProfile) {
		out = append(out, DefaultProfile)
	}
	sort.Strings(out)
	return out, nil
}

// ProfileTokenDir returns the effective token directory for name.
func ProfileTokenDir(name string) (string, error) {
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	return tokenDirForProfile(anthropicProfileEnvVar, anthropicProfileSubdir, name), nil
}

// ValidateProfileName enforces a path-safe, cross-platform profile name.
func ValidateProfileName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("profile name is required")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid profile name %q", name)
	}
	if !profileNameRE.MatchString(name) {
		return fmt.Errorf("invalid profile name %q", name)
	}
	if _, ok := reservedNames[strings.ToLower(name)]; ok {
		return fmt.Errorf("reserved profile name %q", name)
	}
	return nil
}

// LegacyDefaultStatus describes whether a legacy single-store auth.json
// exists at the Anthropic token-dir root and whether the new
// <base>/default/auth.json target already exists.
type LegacyDefaultStatus struct {
	Base         string // the Anthropic token-dir root
	LegacyPath   string // <base>/auth.json
	NewPath      string // <base>/default/auth.json
	LegacyExists bool
	NewExists    bool
	NeedsMigrate bool // LegacyExists && !NewExists
}

// CheckLegacyDefault reports the state of the legacy <base>/auth.json
// relative to the new <base>/default/auth.json layout. It does not modify
// anything; callers use this to print status or decide whether to migrate.
//
// Returns an error if either path can be stat'd to neither "exists" nor
// "ErrNotExist" — e.g. EACCES from a restored backup with wrong ownership.
// Treating such an error as "absent" would silently no-op a real migration.
func CheckLegacyDefault() (LegacyDefaultStatus, error) {
	base := anthropicStore.baseDir()
	s := LegacyDefaultStatus{
		Base:       base,
		LegacyPath: filepath.Join(base, "auth.json"),
		NewPath:    filepath.Join(base, DefaultProfile, "auth.json"),
	}
	if exists, err := statExists(s.LegacyPath); err != nil {
		return s, fmt.Errorf("stat %s: %w", s.LegacyPath, err)
	} else {
		s.LegacyExists = exists
	}
	if exists, err := statExists(s.NewPath); err != nil {
		return s, fmt.Errorf("stat %s: %w", s.NewPath, err)
	} else {
		s.NewExists = exists
	}
	s.NeedsMigrate = s.LegacyExists && !s.NewExists
	return s, nil
}

// statExists returns true if path exists, false if it cleanly doesn't, and an
// error for any other stat failure (EACCES, ELOOP, EIO, …). Conflating those
// with "absent" would hide real problems.
func statExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// MigrateLegacyDefault moves <base>/auth.json into <base>/default/auth.json,
// converting a legacy single-store layout into a named "default" profile.
// After a successful migration, tokenDirForProfile("default") no longer
// depends on filesystem state — it deterministically returns <base>/default.
//
// Returns nil with NeedsMigrate=false in the status if there's nothing to do
// (no legacy file, or the new file already exists). Returns an error if a
// rename collision is detected (both files exist) so the caller can decide.
//
// Symlink note: os.Rename moves the symlink itself, not its target. If the
// legacy auth.json was a symlink, the symlink (not its target) ends up at
// <base>/default/auth.json — credentials still flow, but anyone pointing at
// the old path directly will need to update.
func MigrateLegacyDefault() (LegacyDefaultStatus, error) {
	s, err := CheckLegacyDefault()
	if err != nil {
		return s, err
	}
	if !s.LegacyExists {
		return s, nil
	}
	if s.NewExists {
		return s, fmt.Errorf("migrate: both %s and %s exist; refusing to overwrite — remove one manually", s.LegacyPath, s.NewPath)
	}
	if err := os.MkdirAll(filepath.Join(s.Base, DefaultProfile), 0o700); err != nil {
		return s, fmt.Errorf("migrate: mkdir default dir: %w", err)
	}
	if err := os.Rename(s.LegacyPath, s.NewPath); err != nil {
		return s, fmt.Errorf("migrate: rename %s → %s: %w", s.LegacyPath, s.NewPath, err)
	}
	s.LegacyExists = false
	s.NewExists = true
	s.NeedsMigrate = false
	return s, nil
}

func tokenDirForProfile(envVar, subdir, profile string) string {
	base := defaultTokenDir(envVar, subdir)
	if profile == "" {
		profile = DefaultProfile
	}
	profileDir := filepath.Join(base, profile)
	if profile == DefaultProfile {
		legacy := filepath.Join(base, "auth.json")
		if _, err := os.Stat(legacy); err == nil {
			if _, err := os.Stat(filepath.Join(profileDir, "auth.json")); errors.Is(err, os.ErrNotExist) {
				return base
			}
		}
	}
	return profileDir
}
