package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// Where the proto files are and where the page goes, from services/core.
var (
	protoRoot        = filepath.Join("..", "..", "proto")
	intelligencePage = filepath.Join("internal", "apidocs", "assets", "intelligence.html")
)

const protoPackage = "mhai.intelligence.v1"

// Public paths of the two docs pages. internal/apidocs owns the routes; a
// test there checks these agree with its constants.
const (
	corePagePath         = "/api/docs"
	intelligencePagePath = "/api/docs/intelligence"
	logoPath             = "/api/docs/logo.png"
)

type opView struct {
	Name, Kind, Subject, Request, Response string
	Callers                                []string
	BFFDirect, NeedsHospital, TestOnly     bool
	LimitLabel, Limit                      string
}

type fieldView struct {
	Name, Type, Link, Comment string
	Number                    int
}

type msgView struct {
	Name, Full, File, Comment string
	Fields                    []fieldView
}

type pageData struct {
	CorePath, Path, Logo string
	Ops                  []opView
	Msgs                 []msgView
	Errors               []authz.ErrorCode
}

// compileProtos reads every .proto under protoRoot/mhai/intelligence/v1 with
// comments, so the docs carry what the schema says.
func compileProtos() ([]protoreflect.FileDescriptor, error) {
	names, err := filepath.Glob(filepath.Join(protoRoot, "mhai", "intelligence", "v1", "*.proto"))
	if err != nil || len(names) == 0 {
		return nil, fmt.Errorf("no .proto files under %s (err %v)", protoRoot, err)
	}
	slices.Sort(names)
	var rel []string
	for _, n := range names {
		r, err := filepath.Rel(protoRoot, n)
		if err != nil {
			return nil, err
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	c := protocompile.Compiler{
		Resolver:       protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{protoRoot}}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	files, err := c.Compile(context.Background(), rel...)
	if err != nil {
		return nil, err
	}
	out := make([]protoreflect.FileDescriptor, len(files))
	for i, f := range files {
		out[i] = f
	}
	return out, nil
}

func comment(fd protoreflect.FileDescriptor, d protoreflect.Descriptor) string {
	return strings.Join(strings.Fields(fd.SourceLocations().ByDescriptor(d).LeadingComments), " ")
}

func typeName(f protoreflect.FieldDescriptor) (text, link string) {
	one := func(k protoreflect.FieldDescriptor) (string, string) {
		switch k.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			full := string(k.Message().FullName())
			if strings.HasPrefix(full, protoPackage+".") {
				return strings.TrimPrefix(full, protoPackage+"."), "#msg-" + full
			}
			return full, ""
		case protoreflect.EnumKind:
			return string(k.Enum().FullName()), ""
		}
		return k.Kind().String(), ""
	}
	if f.IsMap() {
		k, _ := one(f.MapKey())
		v, vl := one(f.MapValue())
		return "map<" + k + ", " + v + ">", vl
	}
	t, l := one(f)
	if f.Cardinality() == protoreflect.Repeated {
		t = "repeated " + t
	}
	return t, l
}

func messages(files []protoreflect.FileDescriptor) []msgView {
	var out []msgView
	for _, fd := range files {
		ms := fd.Messages()
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			if m.IsMapEntry() {
				continue
			}
			mv := msgView{
				Name: string(m.Name()), Full: string(m.FullName()),
				File: string(fd.Path()), Comment: comment(fd, m),
			}
			fs := m.Fields()
			for j := 0; j < fs.Len(); j++ {
				f := fs.Get(j)
				t, l := typeName(f)
				mv.Fields = append(mv.Fields, fieldView{
					Name: string(f.Name()), Number: int(f.Number()), Type: t, Link: l, Comment: comment(fd, f),
				})
			}
			out = append(out, mv)
		}
	}
	return out
}

func duration(d time.Duration) string {
	switch {
	case d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	case d%time.Second == 0:
		return plural(int(d/time.Second), "second")
	}
	return d.String()
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// operations builds the rows from the operations table. A request or response
// type that no .proto declares is an error: the table and the schema must agree.
func operations(ops []authz.Operation, msgs []msgView) ([]opView, error) {
	known := map[string]bool{}
	for _, m := range msgs {
		known[m.Full] = true
	}
	out := make([]opView, 0, len(ops))
	for _, o := range ops {
		for _, s := range []string{o.RequestSchema, o.ResponseSchema} {
			if s != "" && !known[s] {
				return nil, fmt.Errorf("operation %s names message %q, which no .proto declares", o.Name, s)
			}
		}
		v := opView{
			Name: o.Name, Request: o.RequestSchema, Response: o.ResponseSchema,
			BFFDirect: o.BFFDirect, NeedsHospital: o.NeedsHospital, TestOnly: o.TestOnly,
			Callers: slices.Clone(o.AnyRole), Limit: duration(o.Timeout),
		}
		if o.SvcCore {
			v.Callers = append(v.Callers, "svc:core")
		}
		if o.Kind == reqtoken.KindJob {
			v.Kind, v.LimitLabel = "job", "Deadline"
			v.Subject = "jobs.v1.<hospital_id>." + o.Op
		} else {
			v.Kind, v.LimitLabel = "request/reply", "Timeout"
			v.Subject = "inference.v1." + o.Service + "." + o.Op
		}
		out = append(out, v)
	}
	return out, nil
}

// intelligenceHTML renders the Intelligence API page: static, no script, no
// external reference.
func intelligenceHTML() ([]byte, error) {
	files, err := compileProtos()
	if err != nil {
		return nil, err
	}
	msgs := messages(files)
	ops, err := operations(authz.Operations(), msgs)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = intelligenceTmpl.Execute(&b, pageData{
		CorePath: corePagePath, Path: intelligencePagePath, Logo: logoPath,
		Ops: ops, Msgs: msgs, Errors: authz.ErrorCodes(),
	})
	return b.Bytes(), err
}

var intelligenceTmpl = template.Must(template.New("intelligence").Funcs(template.FuncMap{
	"yn": func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	},
}).Parse(`<!doctype html>
<!-- Generated by services/core/cmd/permgen from proto/ and internal/authz/operations.go. Do not edit by hand. -->
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>MHAI intelligence API</title>
<style>
body{margin:0;font:16px/1.5 system-ui,sans-serif;color:#1b2430;background:#fff}
nav.tabs{display:flex;gap:4px;align-items:center;padding:8px 16px;background:#f2f5f8;border-bottom:1px solid #d5dce4}
nav.tabs img{height:28px;margin-right:12px}
nav.tabs a{padding:6px 14px;border-radius:6px;color:#1b2430;text-decoration:none}
nav.tabs a:hover{background:#e3e9ef}
nav.tabs a[aria-current="page"]{background:#1b2430;color:#fff}
nav.tabs a:focus-visible,a:focus-visible{outline:3px solid #1a5fb4;outline-offset:2px}
main{max-width:60rem;margin:0 auto;padding:16px}
h2{margin-top:2rem;border-bottom:1px solid #d5dce4}
section.op,section.msg{margin:1rem 0;padding:12px 16px;border:1px solid #d5dce4;border-radius:8px}
dl{display:grid;grid-template-columns:max-content 1fr;gap:4px 16px;margin:0}
dt{font-weight:600}dd{margin:0}
code{font:14px ui-monospace,monospace;background:#f2f5f8;padding:1px 4px;border-radius:4px}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;vertical-align:top;padding:4px 8px;border-bottom:1px solid #e3e9ef}
.tag{display:inline-block;padding:0 8px;border-radius:10px;background:#fde8c8;font-size:14px}
a{color:#1a5fb4}
</style>
</head>
<body>
<nav class="tabs" aria-label="API documentation">
<img src="{{.Logo}}" alt="Measured Health">
<a href="{{.CorePath}}">Core API (HTTP)</a>
<a href="{{.Path}}" aria-current="page">Intelligence API (NATS)</a>
</nav>
<main>
<h1>Intelligence API (NATS)</h1>
<p>The intelligence service has no HTTP API. Its operations are reached over NATS, never over HTTP, and only core (and, for the operations marked <em>BFF direct</em>, the clinical BFF after a grant from core) may call them. Every message carries a core-signed request token; a worker checks it before it reads the body.</p>
<p>This page is generated from the <code>.proto</code> files in <code>proto/</code> and core's operations table. Subjects, users and streams are described in <code>services/nats/README.md</code>.</p>

<h2 id="operations">Operations</h2>
{{range .Ops}}<section class="op" id="op-{{.Name}}" aria-labelledby="h-{{.Name}}">
<h3 id="h-{{.Name}}"><code>{{.Name}}</code>{{if .TestOnly}} <span class="tag">test only</span>{{end}}</h3>
<dl>
<dt>Kind</dt><dd>{{.Kind}}</dd>
<dt>Subject</dt><dd><code>{{.Subject}}</code></dd>
<dt>Request</dt><dd><a href="#msg-{{.Request}}"><code>{{.Request}}</code></a></dd>
{{if .Response}}<dt>Response</dt><dd><a href="#msg-{{.Response}}"><code>{{.Response}}</code></a></dd>
{{else}}<dt>Response</dt><dd>none (a job has no reply message; its outcome is recorded by core)</dd>
{{end}}<dt>Who may call</dt><dd>{{range $i, $c := .Callers}}{{if $i}}, {{end}}<code>{{$c}}</code>{{else}}nobody{{end}}</dd>
<dt>BFF direct</dt><dd>{{yn .BFFDirect}}</dd>
<dt>{{.LimitLabel}}</dt><dd>{{.Limit}}</dd>
<dt>Needs a hospital</dt><dd>{{yn .NeedsHospital}}</dd>
</dl>
</section>
{{end}}
<h2 id="how-checked">How a call is checked</h2>
<p>Before any work, a worker checks, in order: sizes, the token's shape and signature, its claims, its binding to the subject and body, the reply subject, replay, the schema, the scope (every hospital and patient id in the body matches the token) and finally the permission: the operation's roles above must include one of the caller's roles, or <code>svc:core</code> for a service call. A failed check does no work and answers with one of the error codes below. See spec section 7.3 (<code>docs/specs/2026-10-28-intelligence-boundary-design.md</code>).</p>

<h2 id="errors">Error codes</h2>
<p>A worker's <code>Error</code> message holds one of these codes and a retry hint, and no free text.</p>
<table>
<thead><tr><th scope="col">Code</th><th scope="col">Meaning</th></tr></thead>
<tbody>
{{range .Errors}}<tr><td><code>{{.Code}}</code></td><td>{{.Meaning}}</td></tr>
{{end}}</tbody>
</table>

<h2 id="messages">Messages</h2>
{{range .Msgs}}<section class="msg" id="msg-{{.Full}}" aria-labelledby="h-msg-{{.Full}}">
<h3 id="h-msg-{{.Full}}"><code>{{.Full}}</code></h3>
<p>Defined in <code>proto/{{.File}}</code>.{{if .Comment}} {{.Comment}}{{end}}</p>
{{if .Fields}}<table>
<thead><tr><th scope="col">Field</th><th scope="col">Number</th><th scope="col">Type</th><th scope="col">Description</th></tr></thead>
<tbody>
{{range .Fields}}<tr><td><code>{{.Name}}</code></td><td>{{.Number}}</td><td>{{if .Link}}<a href="{{.Link}}"><code>{{.Type}}</code></a>{{else}}<code>{{.Type}}</code>{{end}}</td><td>{{.Comment}}</td></tr>
{{end}}</tbody>
</table>
{{else}}<p>No fields.</p>
{{end}}</section>
{{end}}</main>
</body>
</html>
`))
