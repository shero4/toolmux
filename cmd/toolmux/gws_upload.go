package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The Google Workspace CLI only takes a request body as a single --json
// argument, and Linux caps one argv entry at 128 KiB (MAX_ARG_STRLEN), so a
// Gmail draft or message whose raw RFC 822 payload carries an attachment above
// roughly 90 KB fails with "argument list too long" before the CLI even
// starts. The CLI's --upload mode cannot help: it labels the media
// application/octet-stream, which Gmail rejects. For those requests we borrow
// the CLI's own credentials (gws auth export) and perform the multipart
// upload ourselves, so agents keep using the documented
// body={"message":{"raw":...}} call shape regardless of size.
const gwsArgvLimit = 100 * 1024

const gmailUploadBase = "https://gmail.googleapis.com/upload/gmail/v1/"

type gwsCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// gwsNeedsDirectUpload reports whether the compacted body is too large for the
// CLI and belongs to a Gmail method we can upload directly.
func gwsNeedsDirectUpload(request gwsRequest, body string) bool {
	return len(body) > gwsArgvLimit && request.Service == "gmail"
}

// gmailUploadTarget resolves the upload endpoint for the request. Only the
// methods that accept an RFC 822 payload are supported.
func gmailUploadTarget(request gwsRequest, params map[string]string) (method, path string, err error) {
	userID := params["userId"]
	if userID == "" {
		userID = "me"
	}
	id := params["id"]
	base := "users/" + url.PathEscape(userID)
	switch request.SubResource + "." + request.Method {
	case "drafts.create":
		return http.MethodPost, base + "/drafts", nil
	case "drafts.update":
		if id == "" {
			return "", "", errors.New("drafts.update needs params.id")
		}
		return http.MethodPut, base + "/drafts/" + url.PathEscape(id), nil
	case "messages.send":
		return http.MethodPost, base + "/messages/send", nil
	case "messages.insert":
		return http.MethodPost, base + "/messages", nil
	case "messages.import":
		return http.MethodPost, base + "/messages/import", nil
	}
	return "", "", fmt.Errorf("gmail %s.%s body is %s; the CLI cannot take a body this large and only drafts.create/update and messages.send/insert/import can be uploaded directly", request.SubResource, request.Method, "over 100 KiB")
}

// splitGmailRaw removes message.raw (drafts) or raw (messages) from the body,
// returning the decoded RFC 822 bytes and the remaining metadata JSON.
func splitGmailRaw(request gwsRequest) ([]byte, []byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(request.Body, &body); err != nil {
		return nil, nil, fmt.Errorf("body: %w", err)
	}
	holder := body
	if request.SubResource == "drafts" {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(body["message"], &message); err != nil {
			return nil, nil, errors.New("body.message must be an object with a raw field")
		}
		holder = message
	}
	var encoded string
	if err := json.Unmarshal(holder["raw"], &encoded); err != nil || encoded == "" {
		return nil, nil, errors.New("body is too large for the CLI and carries no raw RFC 822 message to upload")
	}
	delete(holder, "raw")
	rfc822, err := decodeBase64Loose(encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("raw is not valid base64: %w", err)
	}
	if request.SubResource == "drafts" {
		message, err := json.Marshal(holder)
		if err != nil {
			return nil, nil, err
		}
		body["message"] = message
	}
	metadata, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return rfc822, metadata, nil
}

// decodeBase64Loose accepts URL-safe or standard alphabets, with or without
// padding, since Gmail's raw field is base64url but callers vary.
func decodeBase64Loose(value string) ([]byte, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(value), "=")
	if decoded, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(trimmed)
}

func gwsExportCredentials(ctx context.Context, executable, configDir string) (gwsCredentials, error) {
	command := exec.CommandContext(ctx, executable, "auth", "export", "--unmasked")
	command.Env = os.Environ()
	if configDir != "" {
		command.Env = setProcessEnv(command.Env, "GOOGLE_WORKSPACE_CLI_CONFIG_DIR", configDir)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return gwsCredentials{}, fmt.Errorf("export Google Workspace credentials: %w: %s", err, truncateText(strings.TrimSpace(stderr.String()+" "+stdout.String()), 512))
	}
	var credentials gwsCredentials
	if err := json.Unmarshal(stdout.Bytes(), &credentials); err != nil {
		return gwsCredentials{}, fmt.Errorf("parse exported Google Workspace credentials: %w", err)
	}
	if credentials.ClientID == "" || credentials.ClientSecret == "" || credentials.RefreshToken == "" {
		return gwsCredentials{}, errors.New("exported Google Workspace credentials are incomplete")
	}
	return credentials, nil
}

func gwsAccessToken(ctx context.Context, client *http.Client, credentials gwsCredentials) (string, error) {
	form := url.Values{
		"client_id":     {credentials.ClientID},
		"client_secret": {credentials.ClientSecret},
		"refresh_token": {credentials.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("refresh Google access token: %w", err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 300 {
		return "", fmt.Errorf("refresh Google access token: HTTP %d: %s", response.StatusCode, truncateText(string(data), 512))
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.AccessToken == "" {
		return "", errors.New("refresh Google access token: no access_token in response")
	}
	return payload.AccessToken, nil
}

// runGWSGmailUpload performs the request as a multipart/related upload:
// JSON metadata first, then the RFC 822 message. The API response is written
// to stdout exactly as the CLI would have written it.
func runGWSGmailUpload(executable, configDir string, request gwsRequest) error {
	var params map[string]string
	if len(request.Params) > 0 && string(request.Params) != "null" {
		var generic map[string]any
		if err := json.Unmarshal(request.Params, &generic); err != nil {
			return fmt.Errorf("params: %w", err)
		}
		params = make(map[string]string, len(generic))
		for key, value := range generic {
			params[key] = fmt.Sprint(value)
		}
	}
	method, path, err := gmailUploadTarget(request, params)
	if err != nil {
		return err
	}
	rfc822, metadata, err := splitGmailRaw(request)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Minute}
	credentials, err := gwsExportCredentials(ctx, executable, configDir)
	if err != nil {
		return err
	}
	token, err := gwsAccessToken(ctx, client, credentials)
	if err != nil {
		return err
	}

	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return err
	}
	if _, err := part.Write(metadata); err != nil {
		return err
	}
	part, err = writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"message/rfc822"}})
	if err != nil {
		return err
	}
	if _, err := part.Write(rfc822); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	query := url.Values{"uploadType": {"multipart"}}
	for key, value := range params {
		if key != "userId" && key != "id" {
			query.Set(key, value)
		}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, method, gmailUploadBase+path+"?"+query.Encode(), &payload)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Content-Type", "multipart/related; boundary="+writer.Boundary())
	response, err := client.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("gmail upload: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("gmail upload: read response: %w", err)
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("gmail upload: HTTP %d: %s", response.StatusCode, truncateText(strings.TrimSpace(string(data)), 2048))
	}
	_, err = os.Stdout.Write(data)
	return err
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
