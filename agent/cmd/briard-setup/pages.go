package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// THE WINDOW'S WORDS, apart from the Windows calls that show them (window_windows.go): one page
// while the install runs -- a moving bar under the step it is on, since nothing the install runs
// reports how far along it is -- and the page it ends on, chosen by its exit code.

// page is one state of the setup window.
type page struct {
	instruction string // the bold line
	content     string
	details     string // behind "Show details"; none when empty
	failed      bool   // the error icon rather than Briard's own
	restart     bool   // Restart now / Later, rather than Close
	running     bool   // the moving bar, and the one button disabled
}

// detailLines is how much of the install's output a failed page keeps behind "Show details".
const detailLines = 15

var installing = page{instruction: "Installing Briard", content: "Getting ready ...", running: true}

// step is what the window shows of one line of the install's output while it runs: the line as a
// sentence, without what says who wrote it -- install.ps1's "briard: ", or the agent's log stamp and
// its "install: ". Empty for a blank line, which changes nothing.
func step(line string) string {
	l := strings.TrimSpace(line)
	if len(l) > len(logStamp) {
		if _, err := time.Parse(logStamp, l[:len(logStamp)]); err == nil {
			l = l[len(logStamp):]
		}
	}
	l = strings.TrimSpace(l)
	for _, who := range []string{"briard:", "install:"} {
		l = strings.TrimPrefix(l, who)
	}
	return sentence(l)
}

// logStamp is the agent's log line stamp, Go's log.LstdFlags.
const logStamp = "2006/01/02 15:04:05"

// ending is the page the install ends on, from its exit code and the last lines it wrote: 3010
// is installed with the restart owed, 0 installed and starting, anything else a failure, said in
// the install's own last words.
func ending(code int, tail []string, logPath string) page {
	switch code {
	case 3010:
		return page{
			instruction: "Briard is installed",
			content: "Windows must restart once to finish: the network bridge and the hypervisor start with it. " +
				"After the restart, log on: Briard finishes starting and opens your Briard page in your browser.\n\n" +
				"To restart later, choose Restart, not Shut down.",
			restart: true,
		}
	case 0:
		return page{instruction: "Briard is installed", content: "Briard is starting. Open your Briard page from Briard in the Start menu."}
	}
	why := reason(tail)
	if why == "" {
		why = fmt.Sprintf("The install stopped (exit code %d).", code)
	}
	if len(tail) > detailLines {
		tail = tail[len(tail)-detailLines:]
	}
	return page{
		instruction: "The install did not finish",
		content:     why + "\n\nEverything the install did is in " + logPath + ".",
		details:     strings.Join(tail, "\n"),
		failed:      true,
	}
}

// reason is why the install stopped: its last ERROR line's message -- install.ps1's Die and this
// program's own -- or else its last line of install.ps1's, which is how a refusal that is not an
// error (another install running) ends.
func reason(tail []string) string {
	for i := len(tail) - 1; i >= 0; i-- {
		if _, msg, ok := strings.Cut(tail[i], "ERROR: "); ok {
			return period(sentence(msg))
		}
	}
	for i := len(tail) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(tail[i]); strings.HasPrefix(l, "briard: ") {
			return period(step(l))
		}
	}
	return ""
}

// sentence is s trimmed, with its first letter a capital.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return ""
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

func period(s string) string {
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}
