package harness

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

const piErrLog = "/s/child-logs/run-1/wf-" + piToken + ".stderr"

func TestPiSpawnLineIsTheNewForm(t *testing.T) {
	a := For("pi")
	const model = "openrouter/deepseek/deepseek-v4.1-flash-1017"
	line := a.SpawnLine(Spawn{Brief: piBrief, Claim: piToken, Model: model, Effort: "high", ErrLog: piErrLog})
	want := `SUPER_PI_CHILD=wf-` + piToken + ` SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne --session-id wf-` + piToken +
		` --model ` + model + ` --thinking high <<'WF_BRIEF' >/dev/null 2>` + piErrLog + " &\n" + piBrief + "\nWF_BRIEF"
	if line != want {
		t.Fatalf("spawn line:\n got %s\nwant %s", line, want)
	}
	session, brief, errLog, ok := piLaunch(line)
	if !ok || session != "wf-"+piToken || brief != piBrief || errLog != piErrLog {
		t.Fatalf("piLaunch(SpawnLine) = %q, %q, %q, %v", session, brief, errLog, ok)
	}

	glm := a.SpawnLine(Spawn{Brief: piBrief, Claim: piToken, Model: "openrouter/z-ai/glm-5.3-flash", Effort: "off", ErrLog: piErrLog})
	if strings.Contains(glm, "--thinking") {
		t.Fatalf("GLM always thinks: no --thinking off: %s", glm)
	}
	if _, _, _, ok := piLaunch(glm); !ok {
		t.Fatalf("GLM line refused: %s", glm)
	}

	none := a.SpawnLine(Spawn{Brief: piBrief, Claim: piToken})
	if !strings.Contains(none, " >/dev/null 2>/dev/null &\n") {
		t.Fatalf("no ErrLog prints 2>/dev/null: %s", none)
	}
	if _, _, _, ok := piLaunch(none); ok {
		t.Fatalf("piLaunch accepted a line without a stderr file: %s", none)
	}
}

// The brief on stdin is what pi takes as its first message, through bash
// itself: a here-document with single quotes, an @ and a leading - arrives
// verbatim.
func TestPiHeredocCarriesTheBriefVerbatim(t *testing.T) {
	const brief = "-x @f it's \"q\" $HOME `id` \\n\nsecond line"
	line := For("pi").SpawnLine(Spawn{Brief: brief, Claim: piToken, ErrLog: piErrLog})
	_, doc, _ := strings.Cut(line, " &\n")
	cmd := "cat <<'WF_BRIEF'\n" + doc
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil || strings.TrimSuffix(string(out), "\n") != brief {
		t.Fatalf("bash -c %q = %q, %v; want the brief", cmd, out, err)
	}
	if _, b, _, ok := piLaunch(line); !ok || b != brief {
		t.Fatalf("piLaunch = %q, %v", b, ok)
	}
}

func TestPiLaunchGrammar(t *testing.T) {
	const marker = `SUPER_PI_CHILD=wf-k `
	const pre = marker + `SUPER_PI_PARENT="$PI_SESSION_ID" `
	const tail = ` <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\nx --claim t\nWF_BRIEF"
	ok := []string{
		pre + `nohup pi -p -ne --session-id wf-k` + tail,
		pre + `nohup pi --print --no-extensions --model m --thinking low --session-id wf-k` + tail,
		marker + `SUPER_PI_PARENT=${PI_SESSION_ID} nohup /usr/bin/pi -ne -p --session-id wf-k <<'WF_BRIEF' > /dev/null 2> /s/wf-k.stderr &` + "\nx --claim t\nWF_BRIEF\n",
		marker + `SUPER_PI_PARENT="${PI_SESSION_ID}" nohup pi -p -ne --session-id wf-k` + tail,
		pre + `nohup pi -p -ne --session-id wf-k <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\n/example/x/workflow prompt r --claim t\nWF_BRIEF",
		pre + `nohup pi -p -ne --session-id wf-k <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\nx --claim t \"q\" #h && y 'z'\nWF_BRIEF",
	}
	for _, c := range ok {
		if s, b, e, good := piLaunch(c); !good || s != "wf-k" || !strings.HasPrefix(b, "x --claim t") && !strings.HasPrefix(b, "/example/x/workflow") || e != "/s/wf-k.stderr" {
			t.Errorf("piLaunch(%q) = %q, %q, %q, %v", c, s, b, e, good)
		}
	}
	base := `nohup pi -p -ne --session-id wf-k`
	const body = "\nx --claim t\nWF_BRIEF"
	redir := func(r string) string { return pre + base + ` <<'WF_BRIEF' ` + r + body }
	bad := map[string]string{
		"no SUPER_PI_CHILD":               `SUPER_PI_PARENT="$PI_SESSION_ID" ` + base + tail,
		"SUPER_PI_CHILD after the parent": `SUPER_PI_PARENT="$PI_SESSION_ID" ` + marker + base + tail,
		"SUPER_PI_CHILD of another id":    `SUPER_PI_CHILD=wf-other SUPER_PI_PARENT="$PI_SESSION_ID" ` + base + tail,
		"a message argument":              pre + base + ` 'x --claim t'` + tail,
		"a message after --":              pre + base + ` -- 'x'` + tail,
		"the old quoted form":             pre + base + ` -- 'x --claim t' </dev/null >/dev/null 2>/s/wf-k.stderr &`,
		"an unquoted delimiter":           pre + base + ` <<WF_BRIEF >/dev/null 2>/s/wf-k.stderr &` + body,
		"another delimiter":               pre + base + ` <<'EOF' >/dev/null 2>/s/wf-k.stderr &` + "\nx --claim t\nEOF",
		"no closing delimiter":            pre + base + ` <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\nx --claim t",
		"text after the delimiter":        pre + base + tail + "\nrm -rf /",
		"a delimiter inside the brief":    pre + base + ` <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\nx --claim t\nWF_BRIEF\nrm -rf /\nWF_BRIEF",
		"stdin also from /dev/null":       redir(`</dev/null >/dev/null 2>/s/wf-k.stderr &`),
		"a relative stderr path":          redir(`>/dev/null 2>wf-k.stderr &`),
		"a wrongly named stderr file":     redir(`>/dev/null 2>/s/other.stderr &`),
		"a stderr path with a space":      redir(`>/dev/null 2>/s/a b/wf-k.stderr &`),
		"a quoted stderr path":            redir(`>/dev/null 2>'/s/wf-k.stderr' &`),
		"a stderr path with $":            redir(`>/dev/null 2>/s/$X/wf-k.stderr &`),
		"2>/dev/null":                     redir(`>/dev/null 2>/dev/null &`),
		"2>&1":                            redir(`>/dev/null 2>&1 &`),
		"foreground":                      redir(`>/dev/null 2>/s/wf-k.stderr`),
		"no -ne":                          pre + `nohup pi -p --session-id wf-k` + tail,
		"an extension option":             pre + `nohup pi -p -ne -e ./ext.ts --session-id wf-k` + tail,
		"an unknown option":               pre + `nohup pi -p -ne --tools all --session-id wf-k` + tail,
		"no session id":                   pre + `nohup pi -p -ne` + tail,
		"two session ids":                 pre + `nohup pi -p -ne --session-id wf-k --session-id wf-k` + tail,
		"no -p":                           pre + `nohup pi -ne --session-id wf-k` + tail,
		"no nohup":                        pre + `pi -p -ne --session-id wf-k` + tail,
		"no hand-over":                    marker + base + tail,
		"a literal parent":                marker + `SUPER_PI_PARENT=other ` + base + tail,
		"output kept":                     redir(`> log 2>/s/wf-k.stderr &`),
		"a pipe":                          redir(`| tee log &`),
		"a second command":                redir(`>/dev/null 2>/s/wf-k.stderr & rm -rf /`),
		"not pi":                          pre + `nohup notpi -p -ne --session-id wf-k` + tail,
		"a /-expanded prompt":             pre + base + ` <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\n/skill:x --claim t\nWF_BRIEF",
		"a comment hiding a fake suffix":  pre + base + ` # <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + body,
		"a comment after the line":        redir(`>/dev/null 2>/s/wf-k.stderr & # done`),
		"a second command after ;":        redir(`>/dev/null 2>/s/wf-k.stderr; true &`),
		"a doubled background":            redir(`>/dev/null 2>/s/wf-k.stderr & &`),
		"output appended":                 redir(`>>/dev/null 2>/s/wf-k.stderr &`),
		"stderr appended":                 redir(`>/dev/null 2>>/s/wf-k.stderr &`),
		"one line only":                   pre + base + ` <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &`,
		"a glob in the model":             pre + `nohup pi -p -ne --model m* --session-id wf-k` + tail,
		"an unclosed quote":               pre + `nohup pi -p -ne --model 'm --session-id wf-k` + tail,
	}
	for name, c := range bad {
		if s, b, e, good := piLaunch(c); good {
			t.Errorf("%s: piLaunch accepted %q, %q, %q", name, s, b, e)
		}
	}
}

// The exact printed line verifies with and without the leading cd a
// coordinator may add, as piLaunches reads a bash call; anything the shell
// would run differently does not.
func TestPiPrintedLineWithAndWithoutCd(t *testing.T) {
	line := For("pi").SpawnLine(Spawn{Brief: piBrief, Claim: piToken, ErrLog: piErrLog})
	read := func(c string) (string, string, string, bool) {
		first, body, _ := strings.Cut(c, "\n")
		_, rest, ok := piCd(first)
		if !ok {
			return "", "", "", false
		}
		return piLaunch(rest + "\n" + body)
	}
	for _, c := range []string{line, "cd /work/repo && " + line, line + "\n"} {
		if s, b, e, good := read(c); !good || s != "wf-"+piToken || b != piBrief || e != piErrLog {
			t.Errorf("read(%q) = %q, %q, %q, %v", c, s, b, e, good)
		}
	}
	for _, c := range []string{
		strings.Replace(line, " &\n", "\n", 1),
		line + " true",
		line + "\ntrue",
		"cd /work/repo && " + strings.Replace(line, " &\n", " & # x\n", 1),
		"cd /work/repo; " + line,
	} {
		if _, _, _, good := read(c); good {
			t.Errorf("accepted %q", c)
		}
	}
}

func TestPiBriefPredicate(t *testing.T) {
	for _, b := range []string{"/example/x/workflow prompt r --claim c", piBrief, "workflow prompt r\nbody /x", "@f --claim c", "-x --claim c", "it's --claim c", "x\nWF_BRIEFS", "x\nmy WF_BRIEF"} {
		if p := piBriefProblem(b); p != "" {
			t.Errorf("piBriefProblem(%q) = %q, want acceptable", b, p)
		}
	}
	for _, b := range []string{"/review --claim c", "/skill:x --claim c", "/review\n--claim c", "\n /review --claim c", "x\nWF_BRIEF\ny", "WF_BRIEF", "x\nWF_BRIEF \ny", "x\nWF_BRIEF\r\ny", "x\n\tWF_BRIEF\ny"} {
		if piBriefProblem(b) == "" {
			t.Errorf("piBriefProblem(%q) accepted", b)
		}
	}
	line := func(brief string) string {
		return `SUPER_PI_CHILD=wf-k SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne --session-id wf-k <<'WF_BRIEF' >/dev/null 2>/s/wf-k.stderr &` + "\n" + brief + "\nWF_BRIEF"
	}
	if _, _, _, ok := piLaunch(line("/review --claim c")); ok {
		t.Error("piLaunch accepted a brief whose first word is a prompt template")
	}
	if _, _, _, ok := piLaunch(line("/example/x/workflow prompt r --claim c")); !ok {
		t.Error("piLaunch refused an absolute binary path as the first word")
	}
}

func TestPiLaunchMatchesTheRecordedErrLog(t *testing.T) {
	const coord = "coord"
	kid := "wf-" + piToken
	a := For("pi")
	exact := "/state/child-logs/run-1/" + kid + ".stderr"
	wrong := "/wrong/child-logs/run-1/" + kid + ".stderr"
	other := "/other/dir/" + kid + ".stderr"
	handOut := time.Now().Add(-time.Minute).UTC()
	launchAt := handOut.Add(10 * time.Second)
	childAt := launchAt.Add(time.Second)
	line := func(errLog string) string {
		return a.SpawnLine(Spawn{Brief: piBrief, Claim: piToken, ErrLog: errLog})
	}
	setup := func(lines ...string) string {
		root := t.TempDir()
		recs := []map[string]any{piKickoff("run the workflow")}
		for i, l := range lines {
			recs = append(recs, piCommand(l, launchAt.Add(time.Duration(i)*time.Millisecond)))
		}
		piSession(t, root, coord, handOut, recs...)
		piSession(t, root, kid, childAt, piKickoff(piBrief))
		return root
	}
	resolve := func(root, errLog string) bool {
		w := Want{Token: piToken, After: handOut, ErrLog: errLog}
		_, ok := a.Resolve(root, coord, []Want{w})[w]
		return ok
	}

	both := setup(line(wrong), line(exact))
	if !resolve(both, exact) {
		t.Error("the exact launch among two for one claim must verify it")
	}
	if resolve(both, other) {
		t.Error("a launch whose 2> word differs must not verify the claim")
	}
	if !resolve(both, "") {
		t.Error("no recorded ErrLog means no filter")
	}
	onlyWrong := setup(line(wrong))
	if resolve(onlyWrong, exact) {
		t.Error("only a wrong-directory launch: the claim is not verified")
	}
	noLog := setup(line(""))
	if resolve(noLog, "") {
		t.Error("a 2>/dev/null launch never verifies, filter or not")
	}
}

func TestPiStartLogNamesThePath(t *testing.T) {
	s, ok := For("pi").(StartLog)
	if !ok {
		t.Fatal("pi implements StartLog")
	}
	if got := s.StartLog("/s/child-logs/run-1/wf-k.stderr"); !strings.Contains(got, "/s/child-logs/run-1/wf-k.stderr") || !strings.Contains(got, "did not start") {
		t.Fatalf("StartLog = %q", got)
	}
	for _, h := range []string{"claude", "codex", "generic"} {
		if _, ok := For(h).(StartLog); ok {
			t.Errorf("%s must not implement StartLog", h)
		}
	}
}

func TestPiResumeLineMarksTheChild(t *testing.T) {
	const msg = "it's stuck; don't restart"
	line := For("pi").ResumeLine("wf-kid", msg)
	if !strings.HasPrefix(line, piChildMarker("wf-kid")+" ") {
		t.Fatalf("ResumeLine lacks the child marker first: %s", line)
	}
	const suffix = " </dev/null >/dev/null 2>&1 &"
	if !strings.HasSuffix(line, suffix) {
		t.Fatalf("ResumeLine tail: %s", line)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(line, piChildMarker("wf-kid")+" "), suffix)
	argv, ok := literalShellArgv(strings.TrimPrefix(body, `SUPER_PI_PARENT="$PI_SESSION_ID" `))
	if !ok || len(argv) < 3 || argv[len(argv)-1] != msg || argv[len(argv)-2] != "--" {
		t.Fatalf("ResumeLine = %s -> %q, %v", line, argv, ok)
	}
}
