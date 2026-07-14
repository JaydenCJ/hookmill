package cli

import (
	"fmt"
	"io"
	"time"
)

func cmdEndpoint(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "usage: hookmill endpoint add|list|remove|rotate …")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdEndpointAdd(rest, stdout, stderr)
	case "list":
		return cmdEndpointList(rest, stdout, stderr)
	case "remove":
		return cmdEndpointRemove(rest, stdout, stderr)
	case "rotate":
		return cmdEndpointRotate(rest, stdout, stderr)
	default:
		return usageErr(stderr, "unknown endpoint subcommand %q (want add, list, remove, or rotate)", sub)
	}
}

func cmdEndpointAdd(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("endpoint add", stderr)
	url := fs.String("url", "", "delivery URL (required, absolute http/https)")
	secret := fs.String("secret", "", "signing secret (default: generate one)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseErr(err)
	}
	if len(pos) != 1 {
		return usageErr(stderr, "usage: hookmill endpoint add <name> --url URL [--secret S]")
	}
	if *url == "" {
		return usageErr(stderr, "endpoint add: --url is required")
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	ep, err := s.AddEndpoint(pos[0], *url, *secret, time.Now())
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "endpoint %s\n", ep.Name)
	fmt.Fprintf(stdout, "  url     %s\n", ep.URL)
	fmt.Fprintf(stdout, "  secret  %s\n", ep.Secrets[0])
	fmt.Fprintln(stdout, "store the secret in your receiver; hookmill signs every delivery with it")
	return exitOK
}

func cmdEndpointList(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("endpoint list", stderr)
	format := fs.String("format", "text", "output format: text or json")
	showSecrets := fs.Bool("show-secrets", false, "include secrets in the output")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	eps := s.EndpointList()
	if *format == "json" {
		type row struct {
			Name      string    `json:"name"`
			URL       string    `json:"url"`
			Secrets   []string  `json:"secrets,omitempty"`
			NumSecret int       `json:"num_secrets"`
			CreatedAt time.Time `json:"created_at"`
		}
		rows := make([]row, 0, len(eps))
		for _, ep := range eps {
			r := row{Name: ep.Name, URL: ep.URL, NumSecret: len(ep.Secrets), CreatedAt: ep.CreatedAt}
			if *showSecrets {
				r.Secrets = ep.Secrets
			}
			rows = append(rows, r)
		}
		return printJSON(stdout, rows)
	}
	if len(eps) == 0 {
		fmt.Fprintln(stdout, "no endpoints (add one with `hookmill endpoint add`)")
		return exitOK
	}
	fmt.Fprintf(stdout, "%-16s %-40s %s\n", "name", "url", "secrets")
	for _, ep := range eps {
		secrets := fmt.Sprintf("%d", len(ep.Secrets))
		if *showSecrets {
			secrets = fmt.Sprintf("%v", ep.Secrets)
		}
		fmt.Fprintf(stdout, "%-16s %-40s %s\n", ep.Name, ep.URL, secrets)
	}
	return exitOK
}

func cmdEndpointRemove(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("endpoint remove", stderr)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseErr(err)
	}
	if len(pos) != 1 {
		return usageErr(stderr, "usage: hookmill endpoint remove <name>")
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	deadLettered, err := s.RemoveEndpoint(pos[0], time.Now())
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "removed endpoint %s", pos[0])
	if deadLettered > 0 {
		fmt.Fprintf(stdout, " (%s dead-lettered)", plural(int64(deadLettered), "pending message"))
	}
	fmt.Fprintln(stdout)
	return exitOK
}

func cmdEndpointRotate(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("endpoint rotate", stderr)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseErr(err)
	}
	if len(pos) != 1 {
		return usageErr(stderr, "usage: hookmill endpoint rotate <name>")
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	secret, err := s.RotateSecret(pos[0], time.Now())
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "rotated secret for %s\n", pos[0])
	fmt.Fprintf(stdout, "  new secret  %s\n", secret)
	fmt.Fprintln(stdout, "deliveries are now signed with both the new and the previous secret;")
	fmt.Fprintln(stdout, "switch your receiver to the new one, then rotate again to retire the old")
	return exitOK
}
