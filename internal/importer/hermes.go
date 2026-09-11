package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shero4/toolmux/internal/discovery"
	"github.com/shero4/toolmux/internal/store"
)

type repository interface {
	EnsureDiscoveredAgent(context.Context, string, string, string, string, string, string, string) (store.Agent, string, bool, error)
	IssueAgentToken(context.Context, string, string) (string, error)
	RevokeAgentToken(context.Context, string) error
	AgentTokenActive(context.Context, string, string) (bool, error)
	ImportConnection(context.Context, store.ImportedConnection) (string, bool, error)
	EnsureCommandTool(context.Context, string, string, string, string, json.RawMessage, store.CommandToolSpec) error
}

type Manager struct {
	store     repository
	discovery *discovery.Scanner
	baseURL   string
}

type Inventory struct {
	Profiles []Profile
	GWS      []GWSConnection
}

type Profile struct {
	Candidate discovery.Candidate
	Servers   []Server
}

type Server struct {
	Name, Kind, Endpoint, Authorization string
	Enabled                             bool
	imported                            store.ImportedConnection
}

type GWSConnection struct {
	Name, Executable string
}

type Summary struct {
	AgentsCreated, ConnectionsCreated, ConnectionsUpdated, ConfigsUpdated int
	ConnectionIDs                                                         []string
	Warnings                                                              []string
}

type rawConfig struct {
	MCPServers map[string]rawServer `yaml:"mcp_servers"`
}

type rawServer struct {
	URL     string            `yaml:"url"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	Headers map[string]string `yaml:"headers"`
	Auth    string            `yaml:"auth"`
	Enabled *bool             `yaml:"enabled"`
}

type tokenFile struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	TokenType    string  `json:"token_type"`
	Scope        string  `json:"scope"`
	ExpiresAt    float64 `json:"expires_at"`
}

type clientFile struct {
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret"`
	TokenAuthMethod string   `json:"token_endpoint_auth_method"`
	RedirectURIs    []string `json:"redirect_uris"`
	Scope           any      `json:"scope"`
}

type metadataFile struct {
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

func New(store repository, scanner *discovery.Scanner, baseURL string) *Manager {
	return &Manager{store: store, discovery: scanner, baseURL: strings.TrimRight(baseURL, "/")}
}

func (m *Manager) Scan(ctx context.Context) (Inventory, error) {
	var inventory Inventory
	seen := make(map[string]bool)
	for _, candidate := range m.discovery.Scan(ctx) {
		if candidate.Runtime != "hermes" || seen[candidate.ConfigPath] {
			continue
		}
		seen[candidate.ConfigPath] = true
		profile, err := readProfile(candidate)
		if err != nil {
			return Inventory{}, err
		}
		inventory.Profiles = append(inventory.Profiles, profile)
	}
	inventory.GWS = discoverGWS()
	return inventory, nil
}

func (m *Manager) ImportAll(ctx context.Context, inventory Inventory) (Summary, error) {
	var summary Summary
	agents := make([]store.Agent, 0, len(inventory.Profiles))
	for _, profile := range inventory.Profiles {
		candidate := profile.Candidate
		agent, token, created, err := m.store.EnsureDiscoveredAgent(ctx, candidate.Name, store.Slug("hermes-"+candidate.Profile), candidate.ID, candidate.Runtime, candidate.Profile, candidate.Environment, candidate.ConfigPath)
		if err != nil {
			return summary, fmt.Errorf("import %s: %w", candidate.Name, err)
		}
		agents = append(agents, agent)
		if created {
			summary.AgentsCreated++
		}
		existingToken, configured, configErr := toolmuxServerToken(candidate.ConfigPath, m.baseURL+"/mcp")
		if configErr != nil {
			summary.Warnings = append(summary.Warnings, candidate.Name+": its configuration could not be checked: "+configErr.Error())
		} else if configured && token == "" {
			active, err := m.store.AgentTokenActive(ctx, agent.ID, existingToken)
			if err != nil {
				return summary, fmt.Errorf("check token for %s: %w", candidate.Name, err)
			}
			configured = active
		}
		if configErr == nil && !configured && token == "" {
			token, err = m.store.IssueAgentToken(ctx, agent.ID, "hermes-import")
			if err != nil {
				return summary, fmt.Errorf("issue token for %s: %w", candidate.Name, err)
			}
		}
		if !configured && token != "" {
			if err := writeToolmuxServer(candidate.ConfigPath, m.baseURL+"/mcp", token); err != nil {
				_ = m.store.RevokeAgentToken(ctx, token)
				summary.Warnings = append(summary.Warnings, candidate.Name+": agent imported, but its configuration could not be updated: "+err.Error())
			} else {
				summary.ConfigsUpdated++
			}
		}
		for _, server := range profile.Servers {
			if !server.Enabled {
				continue
			}
			input := server.imported
			input.AgentID = agent.ID
			id, connectionCreated, err := m.store.ImportConnection(ctx, input)
			if err != nil {
				return summary, fmt.Errorf("import %s / %s: %w", candidate.Name, server.Name, err)
			}
			summary.ConnectionIDs = append(summary.ConnectionIDs, id)
			if connectionCreated {
				summary.ConnectionsCreated++
			} else {
				summary.ConnectionsUpdated++
			}
		}
	}

	executable, err := os.Executable()
	if err != nil {
		return summary, err
	}
	for _, gws := range inventory.GWS {
		var connectionID string
		for _, agent := range agents {
			input := store.ImportedConnection{
				SourceKey:      "gws:" + gws.Name,
				Kind:           "command",
				ConnectorName:  "Google Workspace CLI",
				ConnectorSlug:  "google-workspace-cli",
				ConnectionName: "Google Workspace · " + strings.TrimPrefix(gws.Name, "gws-"),
				ConnectionSlug: store.Slug(gws.Name),
				AuthMethod:     "none",
				AgentID:        agent.ID,
			}
			id, created, err := m.store.ImportConnection(ctx, input)
			if err != nil {
				return summary, fmt.Errorf("import %s: %w", gws.Name, err)
			}
			connectionID = id
			if created {
				summary.ConnectionsCreated++
			} else if agent.ID == agents[0].ID {
				summary.ConnectionsUpdated++
			}
		}
		if connectionID == "" {
			continue
		}
		args, _ := json.Marshal([]string{"gws-call", gws.Executable})
		if err := m.store.EnsureCommandTool(ctx, connectionID, "request", "Call Google Workspace", "Calls an authorized Google Workspace API through this local identity.", gwsSchema(), store.CommandToolSpec{
			Executable: executable, ArgsTemplate: args, StdinMode: "json", TimeoutMS: 60_000, MaxOutputBytes: 4 << 20,
		}); err != nil {
			return summary, fmt.Errorf("configure %s: %w", gws.Name, err)
		}
		summary.ConnectionIDs = append(summary.ConnectionIDs, connectionID)
	}
	return summary, nil
}

func readProfile(candidate discovery.Candidate) (Profile, error) {
	data, err := os.ReadFile(candidate.ConfigPath)
	if err != nil {
		return Profile{}, err
	}
	var config rawConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return Profile{}, fmt.Errorf("parse %s: %w", candidate.ConfigPath, err)
	}
	profile := Profile{Candidate: candidate}
	names := make([]string, 0, len(config.MCPServers))
	for name := range config.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(name, "toolmux") {
			continue
		}
		raw := config.MCPServers[name]
		server, err := importServer(candidate, name, raw)
		if err != nil {
			return Profile{}, err
		}
		profile.Servers = append(profile.Servers, server)
	}
	return profile, nil
}

func importServer(candidate discovery.Candidate, name string, raw rawServer) (Server, error) {
	enabled := raw.Enabled == nil || *raw.Enabled
	input := store.ImportedConnection{
		SourceKey:      candidate.ID + ":" + name,
		ConnectorName:  title(name),
		ConnectorSlug:  store.Slug("hermes-" + name),
		ConnectionName: title(candidate.Profile) + " · " + title(name),
		ConnectionSlug: store.Slug("hermes-" + candidate.Profile + "-" + name),
		AuthMethod:     "none",
	}
	server := Server{Name: name, Endpoint: raw.URL, Enabled: enabled, Authorization: "Host configuration"}
	if raw.Command != "" {
		args, err := json.Marshal(raw.Args)
		if err != nil {
			return Server{}, err
		}
		input.Kind = "mcp_stdio"
		input.Credential.Environment = raw.Env
		input.Stdio = &store.MCPStdioSpec{Executable: raw.Command, Args: args, WorkingDirectory: filepath.Dir(candidate.ConfigPath)}
		server.Kind = "MCP command"
		server.Authorization = environmentLabel(raw.Env)
	} else {
		input.Kind = "mcp_http"
		input.EndpointURL = raw.URL
		server.Kind = "Remote MCP"
		if strings.EqualFold(raw.Auth, "oauth") {
			credential, oauthConfig := readOAuth(candidate.ConfigPath, name)
			if credential.AccessToken == "" && oauthConfig.AuthorizationURL == "" && oauthConfig.TokenURL == "" {
				server.Authorization = "Provider-managed authorization"
			} else {
				input.AuthMethod = "oauth2"
				input.Credential, input.OAuth = credential, &oauthConfig
				server.Authorization = "OAuth authorization needed"
				if credential.AccessToken != "" {
					server.Authorization = "OAuth imported"
				}
			}
		} else if header, value := singleHeader(raw.Headers); header != "" {
			if strings.EqualFold(header, "Authorization") && strings.HasPrefix(strings.ToLower(value), "bearer ") {
				input.AuthMethod = "bearer"
				input.Credential.BearerToken = strings.TrimSpace(value[len("Bearer "):])
			} else {
				input.AuthMethod = "header"
				input.AuthName = header
				input.Credential.BearerToken = value
			}
			server.Authorization = "Credential imported"
		} else {
			server.Authorization = "No credential"
		}
	}
	server.imported = input
	return server, nil
}

func readOAuth(configPath, name string) (store.Credential, store.OAuthConfig) {
	directory := filepath.Join(filepath.Dir(configPath), "mcp-tokens")
	var token tokenFile
	var client clientFile
	var metadata metadataFile
	readJSON(filepath.Join(directory, name+".json"), &token)
	readJSON(filepath.Join(directory, name+".client.json"), &client)
	readJSON(filepath.Join(directory, name+".meta.json"), &metadata)
	credential := store.Credential{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, TokenType: token.TokenType, ClientSecret: client.ClientSecret}
	if token.ExpiresAt > 0 {
		expires := time.Unix(int64(token.ExpiresAt), int64((token.ExpiresAt-float64(int64(token.ExpiresAt)))*1e9))
		credential.ExpiresAt = &expires
	}
	tokenAuthMethod := client.TokenAuthMethod
	if tokenAuthMethod == "" {
		tokenAuthMethod = "none"
	}
	scopes := token.Scope
	if scopes == "" {
		scopes = scopesFrom(client.Scope, metadata.ScopesSupported)
	}
	redirectURI := ""
	if len(client.RedirectURIs) > 0 {
		redirectURI = client.RedirectURIs[0]
	}
	return credential, store.OAuthConfig{
		AuthorizationURL: metadata.AuthorizationEndpoint,
		TokenURL:         metadata.TokenEndpoint,
		RegistrationURL:  metadata.RegistrationEndpoint,
		ClientID:         client.ClientID,
		Scopes:           scopes,
		TokenAuthMethod:  tokenAuthMethod,
		RedirectURI:      redirectURI,
	}
}

func readJSON(path string, target any) {
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, target)
	}
}

func scopesFrom(value any, fallback []string) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, " ")
	}
	return strings.Join(fallback, " ")
}

func singleHeader(headers map[string]string) (string, string) {
	if len(headers) != 1 {
		return "", ""
	}
	for name, value := range headers {
		return name, value
	}
	return "", ""
}

func environmentLabel(environment map[string]string) string {
	if len(environment) == 0 {
		return "Host environment"
	}
	return fmt.Sprintf("%d environment value(s) imported", len(environment))
}

func discoverGWS() []GWSConnection {
	seen := make(map[string]string)
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := strings.ToLower(entry.Name())
			base := strings.TrimSuffix(strings.TrimSuffix(name, ".cmd"), ".ps1")
			if !strings.HasPrefix(base, "gws-") {
				continue
			}
			if _, ok := seen[base]; !ok || strings.HasSuffix(name, ".cmd") {
				seen[base] = base
			}
		}
	}
	result := make([]GWSConnection, 0, len(seen))
	for name, executable := range seen {
		if path, err := exec.LookPath(executable); err == nil {
			result = append(result, GWSConnection{Name: name, Executable: path})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func gwsSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"},"resource":{"type":"string"},"sub_resource":{"type":"string"},"method":{"type":"string"},"params":{"type":"object"},"body":{"type":"object"},"page_all":{"type":"boolean"},"page_limit":{"type":"integer","minimum":1,"maximum":100}},"required":["service","resource","method"],"additionalProperties":false}`)
}

func writeToolmuxServer(path, endpoint, token string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("configuration root is not a mapping")
	}
	root := document.Content[0]
	servers := mappingValue(root, "mcp_servers")
	if servers == nil {
		servers = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, scalar("mcp_servers"), servers)
	}
	if servers.Kind != yaml.MappingNode {
		return errors.New("mcp_servers is not a mapping")
	}
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		scalar("url"), scalar(endpoint),
		scalar("headers"), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{scalar("Authorization"), scalar("Bearer " + token)}},
		scalar("enabled"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
		scalar("supports_parallel_tool_calls"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	}}
	setMappingValue(servers, "toolmux", entry)

	backup := path + ".toolmux.bak"
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(backup, data, 0o600); err != nil {
			return fmt.Errorf("create backup: %w", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".toolmux-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	encoder := yaml.NewEncoder(temporary)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		temporary.Close()
		return err
	}
	if err := encoder.Close(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, info.Mode().Perm()); err != nil {
		return err
	}
	previous := temporaryPath + ".previous"
	if err := os.Rename(path, previous); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Rename(previous, path)
		return err
	}
	if err := os.Remove(previous); err != nil {
		return fmt.Errorf("remove replaced configuration: %w", err)
	}
	return nil
}

func hasToolmuxServer(path, endpoint string) (bool, error) {
	_, configured, err := toolmuxServerToken(path, endpoint)
	return configured, err
}

func toolmuxServerToken(path, endpoint string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var config rawConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return "", false, err
	}
	server, ok := config.MCPServers["toolmux"]
	value := singleHeaderValue(server.Headers, "Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	configured := ok && server.URL == endpoint && token != "" && token != value
	return token, configured, nil
}

func singleHeaderValue(headers map[string]string, wanted string) string {
	for name, value := range headers {
		if strings.EqualFold(name, wanted) {
			return value
		}
	}
	return ""
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, scalar(key), value)
}

func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func title(value string) string {
	parts := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(value))
	for index := range parts {
		if parts[index] == "" {
			continue
		}
		parts[index] = strings.ToUpper(parts[index][:1]) + parts[index][1:]
	}
	return strings.Join(parts, " ")
}
