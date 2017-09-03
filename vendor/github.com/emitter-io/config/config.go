package config

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"plugin"
	"reflect"
	"strconv"
	"strings"
)

// SecretStore represents a contract for a store capable of resolving secrets.
type SecretStore interface {
	Configure(config Config) error
	GetSecret(secretName string) (string, bool)
}

// Provider represents a configurable provider.
type Provider interface {
	Name() string
	Configure(config map[string]interface{}) error
}

// Config represents a configuration interface.
type Config interface {
	Vault() *VaultConfig
}

// TLSConfig represents TLS listener configuration.
type TLSConfig struct {
	ListenAddr  string `json:"listen"`      // The address to listen on.
	Certificate string `json:"certificate"` // The certificate request.
	PrivateKey  string `json:"private"`     // The private key for the certificate.
}

// Load loads a certificate from the configuration.
func (c *TLSConfig) Load() tls.Certificate {
	// If the certificate provided is in plain text, write to file so we can read it.
	if strings.HasPrefix(c.Certificate, "---") {
		if err := ioutil.WriteFile("broker.crt", []byte(c.Certificate), os.ModePerm); err == nil {
			c.Certificate = "broker.crt"
		}
	}

	// If the private key provided is in plain text, write to file so we can read it.
	if strings.HasPrefix(c.PrivateKey, "---") {
		if err := ioutil.WriteFile("broker.key", []byte(c.PrivateKey), os.ModePerm); err == nil {
			c.PrivateKey = "broker.key"
		}
	}

	// Make sure the paths are absolute, otherwise we won't be able to read the files.
	c.Certificate, _ = filepath.Abs(c.Certificate)
	c.PrivateKey, _ = filepath.Abs(c.PrivateKey)

	// Load the certificate from the cert/key files.
	cert, err := tls.LoadX509KeyPair(c.Certificate, c.PrivateKey)
	if err != nil {
		panic(err)
	}
	return cert
}

// VaultConfig represents Vault configuration.
type VaultConfig struct {
	Address     string `json:"address"` // The vault address to use.
	Application string `json:"app"`     // The vault application ID to use.
}

// ProviderConfig represents provider configuration.
type ProviderConfig struct {

	// The storage provider, this can either be specific builtin or a name of the symbol in
	// the plugin specified by the plugin path.
	Provider string `json:"provider"`

	// The plugin path specifies the location of the plugin which contains the provider.
	PluginPath string `json:"plugin,omitempty"`

	// The configuration for a provider. This specifies various parameters to provide to the
	// specific provider during the Configure() call.
	Config map[string]interface{} `json:"config,omitempty"`
}

// LoadOrPanic loads a provider from the configuration and uses one or several builtins
// provided. If the provider is not found, it panics.
func (c *ProviderConfig) LoadOrPanic(builtins ...Provider) Provider {
	provider, err := c.Load(builtins...)
	if err != nil {
		panic(err)
	}

	return provider
}

// Load loads a provider from the configuration and uses one or several builtins provided.
func (c *ProviderConfig) Load(builtins ...Provider) (Provider, error) {
	if c.PluginPath == "" {
		// Check if a provider configured is a built-in provider
		for _, builtin := range builtins {
			if strings.ToLower(builtin.Name()) == strings.ToLower(c.Provider) {
				if err := builtin.Configure(c.Config); err == nil {
					return builtin, nil
				}
			}
		}

		// Not found a builtin provider
		return nil, errors.New("The provider '" + c.Provider + "' could not be found or configured")
	}

	// Attempt to load a plugin provider
	p, err := plugin.Open(c.PluginPath)
	if err != nil {
		return nil, errors.New("The provider plugin path '" + c.PluginPath + "' could not be opened")
	}

	// Get the symbol
	sym, err := p.Lookup(c.Provider)
	if err != nil {
		return nil, errors.New("The provider '" + c.Provider + "' could not be found in '" + c.PluginPath + "' location")
	}

	// Assert the provider type
	provider, valid := sym.(Provider)
	if !valid {
		return nil, errors.New("The provider '" + c.Provider + "' does not implement Provider interface")
	}

	// Configure the provider
	err = provider.Configure(c.Config)
	if err != nil {
		return nil, errors.New("The provider '" + c.Provider + "' could not be configured")
	}

	// Succesfully opened and configured a provider
	return provider, nil
}

// ClusterConfig represents the configuration for the cluster.
type ClusterConfig struct {

	// The name of this node. This must be unique in the cluster. If this is not set, Emitter
	// will set it to the external IP address of the running machine.
	NodeName string `json:"name,omitempty"`

	// The IP address and port that is used to bind the inter-node communication network. This
	// is used for the actual binding of the port.
	ListenAddr string `json:"listen"`

	// The address and port to advertise inter-node communication network. This is used for nat
	// traversal.
	AdvertiseAddr string `json:"advertise"`

	// The seed address (or a domain name) for cluster join.
	Seed string `json:"seed"`

	// Passphrase is used to initialize the primary encryption key in a keyring. This key
	// is used for encrypting all the gossip messages (message-level encryption).
	Passphrase string `json:"passphrase,omitempty"`
}

// LoadProvider loads a provider from the configuration or panics if the configuration is
// specified, but the provider was not found or not able to configure. This uses the first
// provider as a default value.
func LoadProvider(config *ProviderConfig, providers ...Provider) Provider {
	if config == nil {
		return providers[0]
	}

	// Load the provider according to the configuration
	return config.LoadOrPanic(providers...)
}

// Write writes the configuration to a specific writer, in JSON format.
func write(config interface{}, output io.Writer) (int, error) {
	var formatted bytes.Buffer
	body, err := json.Marshal(config)
	if err != nil {
		return 0, err
	}

	if err := json.Indent(&formatted, body, "", "\t"); err != nil {
		return 0, err
	}

	return output.Write(formatted.Bytes())
}

// createDefault writes the default configuration to disk.
func createDefault(path string, newDefault func() Config) (Config, error) {
	f, err := os.OpenFile(path, os.O_CREATE, os.ModePerm)
	if err != nil {
		return nil, err
	}

	defer f.Close()
	c := newDefault()
	if _, err := write(c, f); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return c, nil
}

// ReadOrCreate reads or creates the configuration object.
func ReadOrCreate(prefix string, path string, newDefault func() Config, stores ...SecretStore) (cfg Config, err error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Create a configuration and write it to a file
		if cfg, err = createDefault(path, newDefault); err != nil {
			return nil, err
		}
	} else {
		// Read the config from file
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// Unmarshal the configuration
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, err
		}
	}

	// Apply all the store overrides, in order
	for _, store := range stores {
		if err := store.Configure(cfg); err == nil {
			declassify(cfg, prefix, store)
		}
	}

	return cfg, nil
}

// Declassify traverses the configuration and resolves secrets.
func declassify(config interface{}, prefix string, provider SecretStore) {
	original := reflect.ValueOf(config)
	declassifyRecursive(prefix, provider, original)
}

// DeclassifyRecursive traverses the configuration and resolves secrets.
func declassifyRecursive(prefix string, provider SecretStore, value reflect.Value) {
	switch value.Kind() {
	case reflect.Ptr:
		if value.Elem().IsValid() {
			declassifyRecursive(prefix, provider, value.Elem())
		}

	// If it is a struct we translate each field
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			name := getFieldName(value.Type().Field(i))
			declassifyRecursive(prefix+"/"+name, provider, value.Field(i))
		}

	// This is a integer, we need to fetch the secret
	case reflect.Int:
		if v, ok := provider.GetSecret(prefix); ok {
			if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
				value.SetInt(iv)
			}
		}

	// This is a string, we need to fetch the secret
	case reflect.String:
		if v, ok := provider.GetSecret(prefix); ok {
			value.SetString(v)
		}
	}
}

func getFieldName(f reflect.StructField) string {
	return strings.Replace(string(f.Tag.Get("json")), ",omitempty", "", -1)
}