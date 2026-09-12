package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/shero4/toolmux/internal/store"
)

type toolsPage struct {
	Tools       []store.Tool
	Connections []store.Connection
	Filter      toolFilter
	Pager       pager
}

// toolForm carries the define-tool form so validation errors can re-render it.
type toolForm struct {
	Connections  []store.Connection
	ConnectionID string
	Kind         string
	Name, Title  string
	Description  string
	InputSchema  string
	// HTTP
	Method, URLTemplate, QueryTemplate, HeadersTemplate, BodyTemplate string
	HTTPTimeout                                                       string
	// Command
	Executable, WorkingDirectory, ArgsTemplate, StdinMode, CredentialEnv string
	CommandTimeout                                                       string
	Error                                                                string
}

type toolPage struct {
	Tool store.ToolDetail
}

func (s *Server) tools(w http.ResponseWriter, r *http.Request) {
	const pageSize = 50
	connections, err := s.store.ListConnections(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	filter := readToolFilter(r)
	filter.Granted = ""
	tools, pagination, err := s.searchToolsPage(r, store.ToolFilter{Query: filter.Query, Kind: filter.Kind, ConnectionID: filter.ConnectionID}, pageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "tools", "connections", "All tools", toolsPage{Tools: tools, Connections: connections, Filter: filter, Pager: pagination})
}

// declarativeConnections lists the connections that accept defined tools.
func (s *Server) declarativeConnections(r *http.Request) ([]store.Connection, error) {
	all, err := s.store.ListConnections(r.Context())
	if err != nil {
		return nil, err
	}
	var result []store.Connection
	for _, connection := range all {
		if connection.Kind == "http_api" || connection.Kind == "command" {
			result = append(result, connection)
		}
	}
	return result, nil
}

func (s *Server) newTool(w http.ResponseWriter, r *http.Request) {
	connections, err := s.declarativeConnections(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	form := toolForm{Connections: connections, ConnectionID: r.URL.Query().Get("connection"), Kind: "http", InputSchema: "{\n  \"type\": \"object\",\n  \"properties\": {}\n}", Method: "GET", QueryTemplate: "{}", HeadersTemplate: "{}", ArgsTemplate: "[]", StdinMode: "none", HTTPTimeout: "60", CommandTimeout: "60"}
	for _, connection := range connections {
		if connection.ID == form.ConnectionID && connection.Kind == "command" {
			form.Kind = "command"
		}
	}
	s.render(w, r, http.StatusOK, "tool_new", "connections", "Define tool", form)
}

func (s *Server) createTool(w http.ResponseWriter, r *http.Request) {
	connections, err := s.declarativeConnections(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	reject := func(form toolForm, message string) {
		form.Connections = connections
		form.Error = message
		s.render(w, r, http.StatusBadRequest, "tool_new", "connections", "Define tool", form)
	}
	if err := r.ParseForm(); err != nil {
		reject(toolForm{}, "The form could not be read.")
		return
	}
	value := func(name string) string { return strings.TrimSpace(r.FormValue(name)) }
	form := toolForm{
		ConnectionID: value("connection_id"), Kind: value("kind"), Name: value("name"), Title: value("title"), Description: value("description"), InputSchema: value("input_schema"),
		Method: strings.ToUpper(value("method")), URLTemplate: value("url_template"), QueryTemplate: value("query_template"), HeadersTemplate: value("headers_template"), BodyTemplate: value("body_template"), HTTPTimeout: value("http_timeout_seconds"),
		Executable: value("executable"), WorkingDirectory: value("working_directory"), ArgsTemplate: value("args_template"), StdinMode: value("stdin_mode"), CredentialEnv: value("credential_env"), CommandTimeout: value("command_timeout_seconds"),
	}
	inputSchema := jsonOrDefault(form.InputSchema, `{"type":"object"}`)
	switch {
	case form.ConnectionID == "":
		reject(form, "Choose the connection this tool belongs to.")
	case !toolNamePattern.MatchString(form.Name):
		reject(form, "Tool names may only contain letters, digits, underscores, and dashes.")
	case !validJSONObject(inputSchema):
		reject(form, "The input schema must be a JSON object.")
	case form.Kind == "http":
		query := jsonOrDefault(form.QueryTemplate, `{}`)
		headers := jsonOrDefault(form.HeadersTemplate, `{}`)
		body := jsonOrDefault(form.BodyTemplate, `null`)
		if !validHTTPMethod(form.Method) {
			reject(form, "Choose an HTTP method.")
			return
		}
		if !validURLTemplate(form.URLTemplate) {
			reject(form, "The URL must be a path starting with / or a complete http(s) URL.")
			return
		}
		if !validStringMap(query) || !validStringMap(headers) || !json.Valid(body) {
			reject(form, "Query and header templates must be JSON string maps, and the body must be valid JSON or blank.")
			return
		}
		spec := store.HTTPToolSpec{Method: form.Method, URLTemplate: form.URLTemplate, QueryTemplate: query, HeadersTemplate: headers, BodyTemplate: body, TimeoutMS: timeoutMS(form.HTTPTimeout, 60), MaxResponseBytes: 4 << 20}
		id, err := s.store.CreateHTTPTool(r.Context(), form.ConnectionID, form.Name, form.Title, form.Description, inputSchema, spec)
		if err != nil {
			s.log.Error("create HTTP tool", "error", err)
			reject(form, "The tool could not be created. Check that the connection is an HTTP API and the name is unused.")
			return
		}
		s.runCheck(r, form.ConnectionID)
		s.redirect(w, r, "/tools/"+id, okFlash("Tool created."))
	case form.Kind == "command":
		args := jsonOrDefault(form.ArgsTemplate, `[]`)
		var values []string
		if form.Executable == "" {
			reject(form, "Enter the executable to run.")
			return
		}
		if json.Unmarshal(args, &values) != nil {
			reject(form, "Arguments must be a JSON array of strings.")
			return
		}
		if form.StdinMode != "none" && form.StdinMode != "json" {
			reject(form, "Choose how input reaches the command.")
			return
		}
		if form.CredentialEnv != "" && !envNamePattern.MatchString(form.CredentialEnv) {
			reject(form, "The credential environment variable must look like GITHUB_TOKEN.")
			return
		}
		spec := store.CommandToolSpec{Executable: form.Executable, WorkingDirectory: form.WorkingDirectory, ArgsTemplate: args, StdinMode: form.StdinMode, CredentialEnv: form.CredentialEnv, TimeoutMS: timeoutMS(form.CommandTimeout, 60), MaxOutputBytes: 4 << 20}
		id, err := s.store.CreateCommandTool(r.Context(), form.ConnectionID, form.Name, form.Title, form.Description, inputSchema, spec)
		if err != nil {
			s.log.Error("create command tool", "error", err)
			reject(form, "The tool could not be created. Check that the connection is an installed command and the name is unused.")
			return
		}
		s.runCheck(r, form.ConnectionID)
		s.redirect(w, r, "/tools/"+id, okFlash("Tool created."))
	default:
		reject(form, "Choose a tool type.")
	}
}

func validHTTPMethod(value string) bool {
	switch value {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

func (s *Server) tool(w http.ResponseWriter, r *http.Request) {
	detail, err := s.store.GetTool(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, "Tool")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "tool", "connections", detail.ExposedName, toolPage{Tool: detail})
}

func (s *Server) deleteTool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.store.GetTool(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "Tool")
		return
	}
	if detail.Kind == "mcp" {
		s.redirect(w, r, "/tools/"+id, errorFlash("Discovered MCP tools are managed by their connection and cannot be deleted individually."))
		return
	}
	if err := s.store.DeleteTool(r.Context(), id); err != nil {
		s.redirect(w, r, "/tools/"+id, errorFlash("The tool could not be deleted."))
		return
	}
	s.redirect(w, r, "/connections/"+detail.ConnectionID, okFlash("Tool "+detail.ExposedName+" deleted."))
}
