package arch

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAppIsTheWord fails on "service" where an app is meant. What a household installs from
// the catalog is an APP everywhere in this repo: in identifiers, wire and file names, messages,
// comments and docs. The word "service" keeps only its other meanings, each spelled out below,
// so a new use reads as one of them or fails here -- the word drifted back once already, which
// is why this is a test and not a convention.
//
// The check removes every allowed use from a line and refuses what is left. To use the word in
// one of its other meanings, write it the way the list below recognises (`systemd service`,
// `service NIC`, `name service`, ...); to add a meaning, add it here with the reason.
func TestAppIsTheWord(t *testing.T) {
	allowed := []*regexp.Regexp{
		// systemd and NixOS: units, their sections and options, and the OS module tree.
		regexp.MustCompile(`[\w@.${}*-]*\.service\b|\[Service\]|serviceConfig|SERVICE_RESULT|systemd\.services|\bservices\.[a-z][\w-]*|(?i)(systemd|transient|OS|oneshot|template|instance) (unit|service)s?\b`),
		// Windows: the service control manager and its API, and the agent's own Windows service.
		regexp.MustCompile(`(?i)windows[- ]services?|service (control )?manager|service controller|\bSCM\b|svc\.\w+|mgr\.\w+|windows/svc|ERROR_SERVICE_\w+|SERVICE_[A-Z_]+|SE_SERVICE|\w*(Create|Open|Close|Delete|Query|Change|Start|Stop|Control|register|open|remove|tap|warden|agent)Service\w*|Service(Name|Running|Stopped|Restart|SpecificExitCode|Handle|Config2?|Status_?)\b|IsWindowsService|runService|serviceSDDL|service(Start|Stop)Bound`),
		// The VIP's side of the guest: the service address, its NIC, tap and MAC.
		regexp.MustCompile(`(?i)service[ -]?(address(es)?|nics?|macs?|taps?|ip|vip|child|identity|network|net|segment|side|link|cidr)\b|\w*Service(Tap|MAC|NIC|Netdev|Link|Address)\w*|\w*service(MAC|Netdev)\b|service[/+ ]+system|system[/+ ]+service|service on net2|service\(eth2\)`),
		// DNS-SD (mDNS) service types, records and instances.
		regexp.MustCompile(`(?i)service[- ]?(types?|records?|instances?|labels?|names? on the lan)\b|ServiceInstance|ServiceType|serviceType|mdnsService|_[a-z-]+\._(tcp|udp)`),
		// python-zeroconf's API, which the rigs' mDNS probes implement.
		regexp.MustCompile(`ServiceBrowser|ServiceListener|ServiceInfo|\b(add|update|remove)_service\b`),
		// HTTP 503, ACME's terms, and the cloud's offerings.
		regexp.MustCompile(`StatusServiceUnavailable|Service Unavailable|TermsOfService\w*|Google Trust Services|netbird service|"\$NB" service run|(?i)name service|managed services?|web services?|cloud services?|paid services?`),
		// Home Assistant's own services (its service calls and its s6 service tree).
		regexp.MustCompile(`/api/services|services\.d|(?i)s6[\w -]*services?|service (call|calls|definitions?|directory|layout)|serviceRoots|homeassistant\.\w+ service`),
		// drbd-reactor's promoter stack, and upstream's shim.
		regexp.MustCompile(`drbd-services@?\w*|drbd-service-shim|stop-services-on-exit`),
		// Third-party words: systemctl's unit type, pion/mdns's ServiceInstance field, and the plain
		// sense of an online service in what a catalog entry says its app does.
		regexp.MustCompile(`--type=service|^\s*Service:\s+s\.typ,|streaming services|apps and services`),
		// The word itself, named as a word in emphasis, where a glossary states the rule.
		regexp.MustCompile(`\*\*?service\*\*?`),
		// Ordinary English: a service as availability, kept or lost.
		regexp.MustCompile(`(?i)in service of|out of service|service level|customer service|self-service|service (gap|interruption|outage)|(lose|loses|lost|restores?|keeps?|withheld) (its |their )?service\b`),
	}
	word := regexp.MustCompile(`(?i)service`)
	windowsOnly := regexp.MustCompile(`_windows(_test)?\.go$|\.ps1$`)

	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".direnv", "vendor", ".release", ".ruff_cache", ".secrets", "static":
				return fs.SkipDir
			}
			if strings.HasPrefix(d.Name(), "result") {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Base(path) == "appword_test.go" || filepath.Base(path) == "LICENSE" {
			return nil // names what it allows; the license is Apache's own text
		}
		// Windows-only code and scripts: there, a service is the Windows service the agent runs as.
		if windowsOnly.MatchString(filepath.Base(path)) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return nil // binary
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			for _, re := range allowed {
				line = re.ReplaceAllString(line, "")
			}
			if word.MatchString(line) {
				t.Errorf("%s:%d says \"service\" where an app is meant: say app, or spell the word's other meaning the way appword_test.go recognises it", rel, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}
