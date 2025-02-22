package age

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
	"github.com/sirupsen/logrus"
	"golang.org/x/term"

	gpgagent "github.com/getsops/gopgagent"
	"github.com/getsops/sops/v3/logging"
)

const (
	// SopsAgeKeyEnv can be set as an environment variable with a string list
	// of age keys as value.
	SopsAgeKeyEnv = "SOPS_AGE_KEY"
	// SopsAgeKeyFileEnv can be set as an environment variable pointing to an
	// age keys file.
	SopsAgeKeyFileEnv = "SOPS_AGE_KEY_FILE"
	// SopsAgeSshPrivateKeyFileEnv can be set as an environment variable pointing to
	// a private SSH key file.
	SopsAgeSshPrivateKeyFileEnv = "SOPS_AGE_SSH_PRIVATE_KEY_FILE"
	// SopsAgeKeyUserConfigPath is the default age keys file path in
	// getUserConfigDir().
	SopsAgeKeyUserConfigPath = "sops/age/keys.txt"
	// On macOS, os.UserConfigDir() ignores XDG_CONFIG_HOME. So we handle that manually.
	xdgConfigHome = "XDG_CONFIG_HOME"
	// KeyTypeIdentifier is the string used to identify an age MasterKey.
	KeyTypeIdentifier = "age"
)

// log is the global logger for any age MasterKey.
var log *logrus.Logger

func init() {
	log = logging.NewLogger("AGE")
}

// MasterKey is an age key used to Encrypt and Decrypt SOPS' data key.
type MasterKey struct {
	// Identity used to contain a Bench32-encoded private key.
	// Deprecated: private keys are no longer publicly exposed.
	// Instead, they are either injected by a (local) key service server
	// using ParsedIdentities.ApplyToMasterKey, or loaded from the runtime
	// environment (variables) as defined by the `SopsAgeKey*` constants.
	Identity string
	// Recipient contains the Bench32-encoded age public key used to Encrypt.
	Recipient string
	// EncryptedKey contains the SOPS data key encrypted with age.
	EncryptedKey string

	// parsedIdentities contains a slice of parsed age identities.
	// It is used to lazy-load the Identities at-most once.
	// It can also be injected by a (local) keyservice.KeyServiceServer using
	// ParsedIdentities.ApplyToMasterKey().
	parsedIdentities []age.Identity
	// parsedRecipient contains a parsed age public key.
	// It is used to lazy-load the Recipient at-most once.
	parsedRecipient age.Recipient
}

// MasterKeysFromRecipients takes a comma-separated list of Bech32-encoded
// public keys, parses them, and returns a slice of new MasterKeys.
func MasterKeysFromRecipients(commaSeparatedRecipients string) ([]*MasterKey, error) {
	if commaSeparatedRecipients == "" {
		// otherwise Split returns [""] and MasterKeyFromRecipient is unhappy
		return make([]*MasterKey, 0), nil
	}
	recipients := strings.Split(commaSeparatedRecipients, ",")

	var keys []*MasterKey
	for _, recipient := range recipients {
		key, err := MasterKeyFromRecipient(recipient)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// MasterKeyFromRecipient takes a Bech32-encoded age public key, parses it, and
// returns a new MasterKey.
func MasterKeyFromRecipient(recipient string) (*MasterKey, error) {
	recipient = strings.TrimSpace(recipient)
	parsedRecipient, err := parseRecipient(recipient)
	if err != nil {
		return nil, err
	}
	return &MasterKey{
		Recipient:       recipient,
		parsedRecipient: parsedRecipient,
	}, nil
}

// ParsedIdentities contains a set of parsed age identities.
// It allows for creating a (local) keyservice.KeyServiceServer which parses
// identities only once, to then inject them using ApplyToMasterKey() for all
// requests.
type ParsedIdentities []age.Identity

// Import attempts to parse the given identities, to then add them to itself.
// It returns any parsing error.
// A single identity argument is allowed to be a multiline string containing
// multiple identities. Empty lines and lines starting with "#" are ignored.
// It is not thread safe, and parallel importing would better be done by
// parsing (using age.ParseIdentities) and appending to the slice yourself, in
// combination with e.g. a sync.Mutex.
func (i *ParsedIdentities) Import(identity ...string) error {
	identities, err := parseIdentities(identity...)
	if err != nil {
		return fmt.Errorf("failed to parse and add to age identities: %w", err)
	}
	*i = append(*i, identities...)
	return nil
}

// ApplyToMasterKey configures the ParsedIdentities on the provided key.
func (i ParsedIdentities) ApplyToMasterKey(key *MasterKey) {
	key.parsedIdentities = i
}

// Encrypt takes a SOPS data key, encrypts it with the Recipient, and stores
// the result in the EncryptedKey field.
func (key *MasterKey) Encrypt(dataKey []byte) error {
	if key.parsedRecipient == nil {
		parsedRecipient, err := parseRecipient(key.Recipient)
		if err != nil {
			log.WithField("recipient", key.parsedRecipient).Info("Encryption failed")
			return err
		}
		key.parsedRecipient = parsedRecipient
	}

	var buffer bytes.Buffer
	aw := armor.NewWriter(&buffer)
	w, err := age.Encrypt(aw, key.parsedRecipient)
	if err != nil {
		log.WithField("recipient", key.parsedRecipient).Info("Encryption failed")
		return fmt.Errorf("failed to create writer for encrypting sops data key with age: %w", err)
	}
	if _, err := w.Write(dataKey); err != nil {
		log.WithField("recipient", key.parsedRecipient).Info("Encryption failed")
		return fmt.Errorf("failed to encrypt sops data key with age: %w", err)
	}
	if err := w.Close(); err != nil {
		log.WithField("recipient", key.parsedRecipient).Info("Encryption failed")
		return fmt.Errorf("failed to close writer for encrypting sops data key with age: %w", err)
	}
	if err := aw.Close(); err != nil {
		log.WithField("recipient", key.parsedRecipient).Info("Encryption failed")
		return fmt.Errorf("failed to close armored writer: %w", err)
	}

	key.SetEncryptedDataKey(buffer.Bytes())
	log.WithField("recipient", key.parsedRecipient).Info("Encryption succeeded")
	return nil
}

// EncryptIfNeeded encrypts the provided SOPS data key, if it has not been
// encrypted yet.
func (key *MasterKey) EncryptIfNeeded(dataKey []byte) error {
	if key.EncryptedKey == "" {
		return key.Encrypt(dataKey)
	}
	return nil
}

// EncryptedDataKey returns the encrypted SOPS data key this master key holds.
func (key *MasterKey) EncryptedDataKey() []byte {
	return []byte(key.EncryptedKey)
}

// SetEncryptedDataKey sets the encrypted SOPS data key for this master key.
func (key *MasterKey) SetEncryptedDataKey(enc []byte) {
	key.EncryptedKey = string(enc)
}

// Decrypt decrypts the EncryptedKey with the parsed or loaded identities, and
// returns the result.
func (key *MasterKey) Decrypt() ([]byte, error) {
	if len(key.parsedIdentities) == 0 {
		ids, err := key.loadIdentities()
		if err != nil {
			log.Info("Decryption failed")
			return nil, fmt.Errorf("failed to load age identities: %w", err)
		}
		ids.ApplyToMasterKey(key)
	}

	src := bytes.NewReader([]byte(key.EncryptedKey))
	ar := armor.NewReader(src)
	r, err := age.Decrypt(ar, key.parsedIdentities...)
	if err != nil {
		log.Info("Decryption failed")
		return nil, fmt.Errorf("failed to create reader for decrypting sops data key with age: %w", err)
	}

	var b bytes.Buffer
	if _, err := io.Copy(&b, r); err != nil {
		log.Info("Decryption failed")
		return nil, fmt.Errorf("failed to copy age decrypted data into bytes.Buffer: %w", err)
	}

	log.Info("Decryption succeeded")
	return b.Bytes(), nil
}

// NeedsRotation returns whether the data key needs to be rotated or not.
func (key *MasterKey) NeedsRotation() bool {
	return false
}

// ToString converts the key to a string representation.
func (key *MasterKey) ToString() string {
	return key.Recipient
}

// ToMap converts the MasterKey to a map for serialization purposes.
func (key *MasterKey) ToMap() map[string]interface{} {
	out := make(map[string]interface{})
	out["recipient"] = key.Recipient
	out["enc"] = key.EncryptedKey
	return out
}

// TypeToIdentifier returns the string identifier for the MasterKey type.
func (key *MasterKey) TypeToIdentifier() string {
	return KeyTypeIdentifier
}

// loadAgeSSHIdentity attempts to load the age SSH identity based on an SSH
// private key from the SopsAgeSshPrivateKeyFileEnv environment variable. If the
// environment variable is not present, it will fall back to `~/.ssh/id_ed25519`
// or `~/.ssh/id_rsa`. If no age SSH identity is found, it will return nil.
func loadAgeSSHIdentity() (age.Identity, error) {
	sshKeyFilePath, ok := os.LookupEnv(SopsAgeSshPrivateKeyFileEnv)
	if ok {
		return parseSSHIdentityFromPrivateKeyFile(sshKeyFilePath)
	}

	userHomeDir, err := os.UserHomeDir()
	if err != nil || userHomeDir == "" {
		log.Warnf("could not determine the user home directory: %v", err)
		return nil, nil
	}

	sshEd25519PrivateKeyPath := filepath.Join(userHomeDir, ".ssh", "id_ed25519")
	if _, err := os.Stat(sshEd25519PrivateKeyPath); err == nil {
		return parseSSHIdentityFromPrivateKeyFile(sshEd25519PrivateKeyPath)
	}

	sshRsaPrivateKeyPath := filepath.Join(userHomeDir, ".ssh", "id_rsa")
	if _, err := os.Stat(sshRsaPrivateKeyPath); err == nil {
		return parseSSHIdentityFromPrivateKeyFile(sshRsaPrivateKeyPath)
	}

	return nil, nil
}

func getUserConfigDir() (string, error) {
	if runtime.GOOS == "darwin" {
		if userConfigDir, ok := os.LookupEnv(xdgConfigHome); ok && userConfigDir != "" {
			return userConfigDir, nil
		}
	}
	return os.UserConfigDir()
}

// loadIdentities attempts to load the age identities based on runtime
// environment configurations (e.g. SopsAgeKeyEnv, SopsAgeKeyFileEnv,
// SopsAgeSshPrivateKeyFileEnv, SopsAgeKeyUserConfigPath). It will load all
// found references, and expects at least one configuration to be present.
func (key *MasterKey) loadIdentities() (ParsedIdentities, error) {
	var identities ParsedIdentities

	sshIdentity, err := loadAgeSSHIdentity()
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH identity: %w", err)
	}
	if sshIdentity != nil {
		identities = append(identities, sshIdentity)
	}

	var readers = make(map[string]io.Reader, 0)

	if ageKey, ok := os.LookupEnv(SopsAgeKeyEnv); ok {
		readers[SopsAgeKeyEnv] = strings.NewReader(ageKey)
	}

	if ageKeyFile, ok := os.LookupEnv(SopsAgeKeyFileEnv); ok {
		f, err := os.Open(ageKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to open %s file: %w", SopsAgeKeyFileEnv, err)
		}
		defer f.Close()
		readers[SopsAgeKeyFileEnv] = f
	}

	userConfigDir, err := getUserConfigDir()
	if err != nil && len(readers) == 0 && len(identities) == 0 {
		return nil, fmt.Errorf("user config directory could not be determined: %w", err)
	}
	if userConfigDir != "" {
		ageKeyFilePath := filepath.Join(userConfigDir, filepath.FromSlash(SopsAgeKeyUserConfigPath))
		f, err := os.Open(ageKeyFilePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to open file: %w", err)
		}
		if errors.Is(err, os.ErrNotExist) && len(readers) == 0 && len(identities) == 0 {
			// If we have no other readers, presence of the file is required.
			return nil, fmt.Errorf("failed to open file: %w", err)
		}
		if err == nil {
			defer f.Close()
			readers[ageKeyFilePath] = f
		}
	}

	for n, r := range readers {

		b := bufio.NewReader(r)
		p, _ := b.Peek(14) // length of "age-encryption" and "-----BEGIN AGE"
		peeked := string(p)

		switch {
		// An age encrypted file, plain or armored.
		case peeked == "age-encryption" || peeked == "-----BEGIN AGE":
			var r io.Reader = b
			if peeked == "-----BEGIN AGE" {
				r = armor.NewReader(r)
			}
			const privateKeySizeLimit = 1 << 24 // 16 MiB
			contents, err := io.ReadAll(io.LimitReader(r, privateKeySizeLimit))
			if err != nil {
				return nil, fmt.Errorf("failed to read '%s': %w", n, err)
			}
			if len(contents) == privateKeySizeLimit {
				return nil, fmt.Errorf("failed to read '%s': file too long", n)
			}
			IncorrectPassphrase := func() {
				conn, err := gpgagent.NewConn()
				if err != nil {
					return
				}
				defer func(conn *gpgagent.Conn) {
					if err := conn.Close(); err != nil {
						log.Errorf("failed to close connection with gpg-agent: %s", err)
					}
				}(conn)
				err = conn.RemoveFromCache(n)
				if err != nil {
					log.Warnf("gpg-agent remove cache request errored: %s", err)
					return
				}
			}
			ids := []age.Identity{&EncryptedIdentity{
				Contents: contents,
				Passphrase: func() (string, error) {
					conn, err := gpgagent.NewConn()
					if err != nil {
						fmt.Fprintf(os.Stderr, "Enter passphrase for identity '%s':", n)

						var pass string
						if term.IsTerminal(int(os.Stdout.Fd())) {
							p, err = term.ReadPassword(int(os.Stdout.Fd()))
							if err == nil {
								pass = string(p)
							}
						} else {
							reader := bufio.NewReader(os.Stdin)
							pass, err = reader.ReadString('\n')
							if err == io.EOF {
								err = nil
							}
						}
						if err != nil {
							return "", fmt.Errorf("could not read passphrase: %v", err)
						}

						fmt.Fprintln(os.Stderr)

						return pass, nil
					}
					defer func(conn *gpgagent.Conn) {
						if err := conn.Close(); err != nil {
							log.Errorf("failed to close connection with gpg-agent: %s", err)
						}
					}(conn)

					req := gpgagent.PassphraseRequest{
						// TODO is the cachekey good enough?
						CacheKey: n,
						Prompt:   "Passphrase",
						Desc:     fmt.Sprintf("Enter passphrase for identity '%s':", n),
					}
					pass, err := conn.GetPassphrase(&req)
					if err != nil {
						return "", fmt.Errorf("gpg-agent passphrase request errored: %s", err)
					}
					//make sure that we won't store empty pass
					if len(pass) == 0 {
						IncorrectPassphrase()
					}
					return pass, nil
				},
				IncorrectPassphrase: IncorrectPassphrase,
				NoMatchWarning: func() {
					log.Warnf("encrypted identity '%s' didn't match file's recipients", n)
				},
			}}
			identities = append(identities, ids...)
		default:
			ids, err := age.ParseIdentities(b)
			if err != nil {
				return nil, fmt.Errorf("failed to parse '%s' age identities: %w", n, err)
			}
			identities = append(identities, ids...)
		}
	}
	return identities, nil
}

// parseRecipient attempts to parse a string containing an encoded age public
// key or a public ssh key.
func parseRecipient(recipient string) (age.Recipient, error) {
	switch {
	case strings.HasPrefix(recipient, "age1"):
		parsedRecipient, err := age.ParseX25519Recipient(recipient)
		if err != nil {
			return nil, fmt.Errorf("failed to parse input as Bech32-encoded age public key: %w", err)
		}

		return parsedRecipient, nil
	case strings.HasPrefix(recipient, "ssh-"):
		parsedRecipient, err := agessh.ParseRecipient(recipient)
		if err != nil {
			return nil, fmt.Errorf("failed to parse input as age-ssh public key: %w", err)
		}
		return parsedRecipient, nil
	}

	return nil, fmt.Errorf("failed to parse input, unknown recipient type: %q", recipient)
}

// parseIdentities attempts to parse the string set of encoded age identities.
// A single identity argument is allowed to be a multiline string containing
// multiple identities. Empty lines and lines starting with "#" are ignored.
func parseIdentities(identity ...string) (ParsedIdentities, error) {
	var identities []age.Identity
	for _, i := range identity {
		parsed, err := age.ParseIdentities(strings.NewReader(i))
		if err != nil {
			return nil, err
		}
		identities = append(identities, parsed...)
	}
	return identities, nil
}

type EncryptedIdentity struct {
	Contents            []byte
	Passphrase          func() (string, error)
	NoMatchWarning      func()
	IncorrectPassphrase func()

	identities []age.Identity
}

var _ age.Identity = &EncryptedIdentity{}

func (i *EncryptedIdentity) Unwrap(stanzas []*age.Stanza) (fileKey []byte, err error) {
	if i.identities == nil {
		if err := i.decrypt(); err != nil {
			return nil, err
		}
	}

	for _, id := range i.identities {
		fileKey, err = id.Unwrap(stanzas)
		if errors.Is(err, age.ErrIncorrectIdentity) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return fileKey, nil
	}
	i.NoMatchWarning()
	return nil, age.ErrIncorrectIdentity
}

func (i *EncryptedIdentity) decrypt() error {
	d, err := age.Decrypt(bytes.NewReader(i.Contents), &LazyScryptIdentity{i.Passphrase})
	if e := new(age.NoIdentityMatchError); errors.As(err, &e) {
		// ScryptIdentity returns ErrIncorrectIdentity for an incorrect
		// passphrase, which would lead Decrypt to returning "no identity
		// matched any recipient". That makes sense in the API, where there
		// might be multiple configured ScryptIdentity. Since in cmd/age there
		// can be only one, return a better error message.
		i.IncorrectPassphrase()
		return fmt.Errorf("incorrect passphrase")
	}
	if err != nil {
		return fmt.Errorf("failed to decrypt identity file: %v", err)
	}
	i.identities, err = age.ParseIdentities(d)
	return err
}

// LazyScryptIdentity is an age.Identity that requests a passphrase only if it
// encounters an scrypt stanza. After obtaining a passphrase, it delegates to
// ScryptIdentity.
type LazyScryptIdentity struct {
	Passphrase func() (string, error)
}

var _ age.Identity = &LazyScryptIdentity{}

func (i *LazyScryptIdentity) Unwrap(stanzas []*age.Stanza) (fileKey []byte, err error) {
	for _, s := range stanzas {
		if s.Type == "scrypt" && len(stanzas) != 1 {
			return nil, errors.New("an scrypt recipient must be the only one")
		}
	}
	if len(stanzas) != 1 || stanzas[0].Type != "scrypt" {
		return nil, age.ErrIncorrectIdentity
	}
	pass, err := i.Passphrase()
	if err != nil {
		return nil, fmt.Errorf("could not read passphrase: %v", err)
	}
	ii, err := age.NewScryptIdentity(pass)
	if err != nil {
		return nil, err
	}
	fileKey, err = ii.Unwrap(stanzas)
	return fileKey, err
}
