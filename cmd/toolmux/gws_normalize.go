package main

import (
	"fmt"
	"strings"
)

// gwsServices are the Google Workspace API names the CLI accepts as the first
// path element. Used to recover the service when a caller puts it elsewhere.
var gwsServices = map[string]bool{
	"gmail": true, "drive": true, "calendar": true, "sheets": true, "docs": true,
	"slides": true, "admin": true, "people": true, "tasks": true, "forms": true,
	"chat": true, "script": true, "keep": true, "meet": true, "classroom": true,
	"groupssettings": true, "reports": true, "licensing": true, "cloudidentity": true,
}

// gmailUserResources live under users/{userId}/ in the Gmail API; callers often
// drop the leading "users".
var gmailUserResources = map[string]bool{
	"messages": true, "threads": true, "drafts": true, "labels": true,
	"settings": true, "history": true,
}

func splitGWSPath(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '.' || r == '/' || r == ' ' }) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// normalizeGWSRequest accepts the shapes models naturally write for a Google
// API call and rewrites them into service / resource / sub_resources / method:
//
//	resource "users.messages", method "get"
//	method "users.messages.get" or "gmail.users.messages.get"
//	service and resource swapped, or the service repeated in resource
//	Gmail resource "messages" without the leading "users"
//
// It never invents a method; the last path element is always the method.
func normalizeGWSRequest(request *gwsRequest) error {
	var tokens []string
	tokens = append(tokens, splitGWSPath(request.Service)...)
	tokens = append(tokens, splitGWSPath(request.Resource)...)
	tokens = append(tokens, splitGWSPath(request.SubResource)...)
	for _, value := range request.SubResources {
		tokens = append(tokens, splitGWSPath(value)...)
	}
	tokens = append(tokens, splitGWSPath(request.Method)...)

	service := ""
	if parts := splitGWSPath(request.Service); len(parts) > 0 && gwsServices[parts[0]] {
		service = parts[0]
	} else {
		for _, token := range tokens {
			if gwsServices[token] {
				service = token
				break
			}
		}
	}
	if service == "" {
		if parts := splitGWSPath(request.Service); len(parts) == 1 {
			service = parts[0]
		} else {
			return fmt.Errorf("service is required, e.g. {\"service\":\"gmail\",\"resource\":\"users\",\"sub_resource\":\"messages\",\"method\":\"list\"}")
		}
	}
	var path []string
	for _, token := range tokens {
		if token == service {
			continue
		}
		path = append(path, token)
	}
	if service == "gmail" && len(path) >= 2 && gmailUserResources[path[0]] {
		path = append([]string{"users"}, path...)
	}
	if len(path) < 2 {
		return fmt.Errorf("need at least a resource and a method after service %q, e.g. resource \"users\", sub_resource \"messages\", method \"list\"", service)
	}
	request.Service = service
	request.Resource = path[0]
	request.Method = path[len(path)-1]
	// Keep the first middle element in SubResource: the Gmail upload path keys on
	// it (drafts / messages). The CLI argv is identical either way.
	middle := path[1 : len(path)-1]
	request.SubResource = ""
	request.SubResources = nil
	if len(middle) > 0 {
		request.SubResource = middle[0]
		request.SubResources = append([]string(nil), middle[1:]...)
	}
	return nil
}
