package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Retty-Software/civitone/controller/internal/access"
)

// signinLink is `civitone signin-link`: the self-hoster's way into a console
// that needs a sign-in. It talks to the running controller's admin API,
// adds an owner to the access document if there's none yet, and prints a
// one-time link.
//
//	docker compose exec civitone civitone signin-link --name "Pat"
func signinLink(args []string) error {
	fs := flag.NewFlagSet("signin-link", flag.ExitOnError)
	base := fs.String("url", "http://127.0.0.1:8080", "the running controller")
	key := fs.String("admin-key", os.Getenv("CIVITONE_ADMIN_KEY"), "its admin key (default: CIVITONE_ADMIN_KEY)")
	person := fs.String("person", "owner", "the person to sign in, by id")
	name := fs.String("name", "Owner", "name for the owner, if one is created")
	fs.Parse(args)
	if *key == "" {
		return fmt.Errorf("no admin key: set CIVITONE_ADMIN_KEY or pass --admin-key")
	}
	call := func(method, path string, body any, out any) error {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, strings.TrimRight(*base, "/")+"/admin/v1"+path, rd)
		req.Header.Set("Authorization", "Bearer "+*key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(msg)))
		}
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}
	var doc access.Doc
	if err := call("GET", "/access", nil, &doc); err != nil {
		return err
	}
	found := false
	for _, p := range doc.People {
		found = found || p.ID == *person
	}
	if !found {
		if *person != "owner" {
			return fmt.Errorf("no person %q in the access document", *person)
		}
		doc.People = append(doc.People, access.Person{ID: "owner", Name: *name, Role: access.Owner})
		if err := call("PUT", "/access", doc, nil); err != nil {
			return err
		}
	}
	var link struct {
		URL string `json:"url"`
	}
	if err := call("POST", "/signin", map[string]string{"person": *person}, &link); err != nil {
		return err
	}
	fmt.Println(link.URL)
	fmt.Fprintln(os.Stderr, "Open it within 5 minutes. It works once.")
	return nil
}
