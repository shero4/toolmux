package main

import (
	"reflect"
	"testing"
)

func TestNormalizeGWSRequest(t *testing.T) {
	cases := []struct {
		name string
		in   gwsRequest
		want []string // service, resource, sub..., method
	}{
		{"canonical", gwsRequest{Service: "gmail", Resource: "users", SubResource: "messages", Method: "list"}, []string{"gmail", "users", "messages", "list"}},
		{"dotted resource", gwsRequest{Service: "gmail", Resource: "users.messages", Method: "get"}, []string{"gmail", "users", "messages", "get"}},
		{"swapped", gwsRequest{Service: "users.messages", Resource: "gmail", Method: "list"}, []string{"gmail", "users", "messages", "list"}},
		{"dotted method", gwsRequest{Service: "gmail", Resource: "gmail", Method: "users.messages.get"}, []string{"gmail", "users", "messages", "get"}},
		{"full method no service", gwsRequest{Resource: "gmail", Method: "gmail.users.messages.get"}, []string{"gmail", "users", "messages", "get"}},
		{"gmail without users", gwsRequest{Service: "gmail", Resource: "messages", Method: "get"}, []string{"gmail", "users", "messages", "get"}},
		{"getProfile", gwsRequest{Service: "gmail", Resource: "users", Method: "getProfile"}, []string{"gmail", "users", "getProfile"}},
		{"settings sendAs", gwsRequest{Service: "gmail", Resource: "users", SubResources: []string{"settings", "sendAs"}, Method: "list"}, []string{"gmail", "users", "settings", "sendAs", "list"}},
		{"nested sub_resource", gwsRequest{Service: "gmail", Resource: "users", SubResource: "settings/sendAs", Method: "list"}, []string{"gmail", "users", "settings", "sendAs", "list"}},
		{"drive", gwsRequest{Service: "drive", Resource: "files", Method: "list"}, []string{"drive", "files", "list"}},
		{"drive dotted", gwsRequest{Service: "drive", Resource: "drive", Method: "files.list"}, []string{"drive", "files", "list"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.in
			if err := normalizeGWSRequest(&r); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := []string{r.Service, r.Resource}
			if r.SubResource != "" {
				got = append(got, r.SubResource)
			}
			got = append(got, r.SubResources...)
			got = append(got, r.Method)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	drafts := gwsRequest{Service: "gmail", Resource: "users.drafts", Method: "create"}
	if err := normalizeGWSRequest(&drafts); err != nil || drafts.SubResource != "drafts" {
		t.Fatalf("drafts upload routing needs SubResource=drafts, got %q (%v)", drafts.SubResource, err)
	}
	bad := gwsRequest{Service: "gmail", Method: "list"}
	if err := normalizeGWSRequest(&bad); err == nil {
		t.Fatal("expected an error when only a method is given")
	}
}
