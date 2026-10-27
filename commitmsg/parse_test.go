package commitmsg

import (
	"os/exec"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, typed := range []string{
		"git commit -m", "git commit -m ", "git commit -m '", "git commit -m \"fix: ",
		"git commit --message", "git commit --message=", "git commit --message='fix: ",
		"git commit -am '", "git commit -a -m '", "git commit --all --message \"",
		"\tgit  commit\t-m  '",
	} {
		t.Run(typed, func(t *testing.T) {
			e, ok := parse(typed)
			if !ok || e.all != strings.Contains(typed, "-a") {
				t.Fatalf("parse = %+v, %v", e, ok)
			}
		})
	}
	for _, typed := range []string{
		"", "git status", "git commit", "git -C /tmp commit -m '", "echo x; git commit -m '",
		"git commit -m done", "git commit -m ''", "git commit -m 'done' && echo x",
		"git commit -m\n", "git commit -m 'a\nb", "git commit -m \"$HOME",
		"git commit -m \"$(touch bad)", "git commit -m \"`id`", "git commit -m \"wow!",
		"git commit -m \"unfinished\\", "git commit -m 'closed' tail", "git commit -mfix",
		"git commit --message-other", "git commit -a -am '", "git commit -m ; echo x",
	} {
		t.Run(typed, func(t *testing.T) {
			if e, ok := parse(typed); ok {
				t.Fatalf("accepted unsupported syntax: %+v", e)
			}
		})
	}
}

func TestShellRoundTrip(t *testing.T) {
	for _, typed := range []string{
		"git commit -m", "git commit --message=", "git commit -m '",
		"git commit -m 'fix: ", "git commit -m \"fix: ",
		"git commit -m \"fix: \\\"quoted\\\" \\$literal \\\\ ",
	} {
		t.Run(typed, func(t *testing.T) {
			e, ok := parse(typed)
			if !ok {
				t.Fatal("parse rejected input")
			}
			subject := e.prefix + "keep 'quotes', \"double\", $HOME, $(exit 77), `exit 78`, \\, !; & | <>"
			completed := e.extend(typed, subject)
			if !strings.HasPrefix(completed, typed) {
				t.Fatalf("changed typed prefix: %q", completed)
			}
			// Replace only the fixed command/options; printf receives the message as data.
			argument := strings.TrimPrefix(completed, "git commit -m ")
			argument = strings.TrimPrefix(argument, "git commit --message=")
			output, err := exec.CommandContext(t.Context(), "sh", "-c", "printf '%s' "+argument).Output()
			if err != nil || string(output) != subject {
				t.Fatalf("shell round trip = %q, %v; want %q", output, err, subject)
			}
		})
	}
}

func TestValidSubject(t *testing.T) {
	if !validSubject("fix: preserve user's $value", "fix: ") {
		t.Fatal("rejected valid subject")
	}
	for _, subject := range []string{
		"", " ", "feat: different", "fix: a\nb", "fix: a\rb", "fix: a\x00b", "fix: a\u2028b",
		" fix: spaces", "fix: spaces ", "fix: ```code```", strings.Repeat("x", 241),
	} {
		if validSubject(subject, "fix: ") {
			t.Errorf("accepted invalid subject %q", subject)
		}
	}
	for _, subject := range []string{
		"git commit -m 'fix'", "$ git commit", "Subject: fix", "Commit message: fix",
		"'fix: quoted'", "\"fix: quoted\"", "~~~sh", "```sh",
	} {
		if validSubject(subject, "") {
			t.Errorf("accepted decorated subject %q", subject)
		}
	}
	if validSubject("fix: lowercase", "Fix: ") {
		t.Fatal("prefix matching ignored case")
	}
}
