// Command mdserver is an http server serving single directory with markdown
// (.md) files. If can render automatically built index of those files and
// render them as html pages.
//
// Its main use-case is reading through directory with documentation written in
// markdown format, i.e. local copy of Github wiki.
//
// To access automatically generated index, request "/?index" path, as
// http://localhost:8080/?index.
//
// To create home page available at / either create index.html file or start
// server with -rootindex flag to render automatically generated index.
//
//
// Note that table of contents generating javascript is a modified version of
// code found at https://github.com/matthewkastor/html-table-of-contents which
// is licensed under GNU GENERAL PUBLIC LICENSE Version 3.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/artyom/autoflags"
	"github.com/artyom/httpgzip"
	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/microcosm-cc/bluemonday"
	"github.com/pkg/browser"
	"golang.org/x/text/language"
	"golang.org/x/text/search"
)

func main() {
	args := runArgs{Dir: ".", Addr: "localhost:8080"}
	autoflags.Parse(&args)
	if err := run(args); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

type runArgs struct {
	Dir  string `flag:"dir,directory with markdown (.md) files"`
	Addr string `flag:"addr,address to listen"`
	Open bool   `flag:"open,open index page in default browser on start"`
	Ghub bool   `flag:"github,rewrite github wiki links to local when rendering"`
	Grep bool   `flag:"search,enable substring search"`
	Idx  bool   `flag:"rootindex,render autogenerated index at / in addition to /?index"`
	CSS  string `flag:"css,path to custom CSS file"`
}

func run(args runArgs) error {
	h := &mdHandler{
		dir:        args.Dir,
		fileServer: http.FileServer(http.Dir(args.Dir)),
		githubWiki: args.Ghub,
		withSearch: args.Grep,
		rootIndex:  args.Idx,
		style:      template.CSS(style),
	}
	if args.CSS != "" {
		b, err := ioutil.ReadFile(args.CSS)
		if err != nil {
			return err
		}
		h.style = template.CSS(b)
	}
	sum := sha256.Sum256([]byte(h.style))
	h.styleHash = "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
	srv := http.Server{
		Addr:        args.Addr,
		Handler:     httpgzip.New(h),
		ReadTimeout: time.Second,
	}
	if args.Open {
		go func() {
			time.Sleep(100 * time.Millisecond)
			browser.OpenURL("http://" + args.Addr + "/?index")
		}()
	}
	return srv.ListenAndServe()
}

type mdHandler struct {
	dir        string
	fileServer http.Handler // initialized as http.FileServer(http.Dir(dir))
	githubWiki bool
	withSearch bool
	rootIndex  bool
	style      template.CSS
	styleHash  string // sha256-{HASH} value for CSP
}

func (h *mdHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	type indexPage struct {
		Title      string
		Style      template.CSS
		Index      []indexRecord
		WithSearch bool
	}
	if h.withSearch && r.URL.Path == "/" && strings.HasPrefix(r.URL.RawQuery, "q=") {
		q := r.URL.Query().Get("q")
		if len(q) < 3 {
			http.Error(w, "Search term is too short", http.StatusBadRequest)
			return
		}
		pat := search.New(language.English, search.Loose).CompileString(q)
		indexTemplate.Execute(w, indexPage{
			Title:      fmt.Sprintf("Search results for %q", q),
			Style:      h.style,
			Index:      dirIndex(h.dir, pat),
			WithSearch: h.withSearch})
		return
	}
	if r.URL.Path == "/" && (h.rootIndex || r.URL.RawQuery == "index") {
		indexTemplate.Execute(w, indexPage{
			Title:      "Index",
			Style:      h.style,
			Index:      dirIndex(h.dir, nil),
			WithSearch: h.withSearch})
		return
	}
	if !strings.HasSuffix(r.URL.Path, ".md") {
		h.fileServer.ServeHTTP(w, r)
		return
	}
	// only markdown files are handled below
	p := path.Clean(r.URL.Path)
	if containsDotDot(p) {
		http.Error(w, "invalid URL path", http.StatusBadRequest)
		return
	}
	name := filepath.Join(h.dir, filepath.FromSlash(p))
	b, err := ioutil.ReadFile(name)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		log.Printf("read %q: %v", name, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self';"+
		"img-src http: https: data:;media-src https:;"+
		"script-src 'sha256-fuJOTtU+swhVjMGahGvof8RbeaIDlptfQDoHubzBL9I=';"+
		"style-src '"+h.styleHash+"';")
	opts := rendererOpts
	if h.githubWiki {
		opts.RenderNodeHook = rewriteGithubWikiLinks
	}
	body := markdown.ToHTML(b, parser.NewWithExtensions(extensions), html.NewRenderer(opts))
	body = policy.SanitizeBytes(body)
	pageTemplate.Execute(w, struct {
		Title string
		Style template.CSS
		Body  template.HTML
	}{
		Title: nameToTitle(filepath.Base(name)),
		Style: h.style,
		Body:  template.HTML(body),
	})
}

func dirIndex(dir string, pat *search.Pattern) []indexRecord {
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		panic(err)
	}
	index := make([]indexRecord, 0, len(matches))
	for _, s := range matches {
		if pat != nil && !matchPattern(pat, s) {
			continue
		}
		file := filepath.Base(s)
		title := documentTitle(s)
		if title == "" {
			title = nameToTitle(file)
		}
		index = append(index, indexRecord{Title: title, File: file})
	}
	return index
}

type indexRecord struct {
	Title, File string
}

// documentTitle extracts h1 header from markdown document
func documentTitle(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := ioutil.ReadAll(io.LimitReader(f, 1<<17))
	if err != nil {
		return ""
	}
	doc := parser.New().Parse(b)
	var title string
	walkFn := func(node ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.GoToNext
		}
		switch n := node.(type) {
		case *ast.Heading:
			if n.Level != 1 {
				return ast.GoToNext
			}
			title = string(childLiterals(n))
			return ast.Terminate
		case *ast.Code, *ast.CodeBlock, *ast.BlockQuote:
			return ast.SkipChildren
		}
		return ast.GoToNext
	}
	_ = ast.Walk(doc, ast.NodeVisitorFunc(walkFn))
	return title
}

func childLiterals(node ast.Node) []byte {
	if l := node.AsLeaf(); l != nil {
		return l.Literal
	}
	var out [][]byte
	for _, n := range node.GetChildren() {
		if lit := childLiterals(n); lit != nil {
			out = append(out, lit)
		}
	}
	if out == nil {
		return nil
	}
	return bytes.Join(out, nil)
}

// matchPattern reports whether any line in file matches given pattern. On any
// errors function return false.
func matchPattern(pat *search.Pattern, file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		if _, end := pat.Index(sc.Bytes()); end > 0 {
			return true
		}
	}
	return false
}

// rewriteGithubWikiLinks is a html.RenderNodeFunc which renders links
// with github wiki destinations as local ones.
//
// Link with "https://github.com/user/project/wiki/Page" destination would be
// rendered as a link to "Page.md"
func rewriteGithubWikiLinks(w io.Writer, node ast.Node, entering bool) (ast.WalkStatus, bool) {
	link, ok := node.(*ast.Link)
	if !ok || !entering {
		return ast.GoToNext, false
	}
	if u, err := url.Parse(string(link.Destination)); err == nil &&
		u.Host == "github.com" && strings.HasSuffix(path.Dir(u.Path), "/wiki") {
		dst := path.Base(u.Path) + ".md"
		switch u.Fragment {
		case "":
			fmt.Fprintf(w, "<a href=\"%s\">", url.QueryEscape(dst))
		default:
			fmt.Fprintf(w, "<a href=\"%s#%s\">", url.QueryEscape(dst), url.QueryEscape(u.Fragment))
		}
		return ast.GoToNext, true
	}
	return ast.GoToNext, false
}

func nameToTitle(name string) string {
	const suffix = ".md"
	if strings.ContainsAny(name, " ") {
		return strings.TrimSuffix(name, suffix)
	}
	return repl.Replace(strings.TrimSuffix(name, suffix))
}

var repl = strings.NewReplacer("-", " ")

var indexTemplate = template.Must(template.New("index").Parse(indexTpl))
var pageTemplate = template.Must(template.New("page").Parse(pageTpl))

const indexTpl = `<!doctype html><head><meta charset="utf-8"><title>{{.Title}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>{{.Style}}</style></head><body>{{if .WithSearch}}<form method="get">
<input type="search" name="q" minlength="3" placeholder="Substring search" autofocus required>
<input type="submit"></form>{{end}}
<h1>{{.Title}}</h1><ul>
{{range .Index}}<li><a href="{{.File}}">{{.Title}}</a></li>
{{end}}</ul></body>
`

const pageTpl = `<!doctype html><head><meta charset="utf-8"><title>{{.Title}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>{{.Style}}</style><script>
document.addEventListener('DOMContentLoaded', function() {
	htmlTableOfContents();
} );
function htmlTableOfContents( documentRef ) {
	var documentRef = documentRef || document;
	var toc = documentRef.getElementById("toc");
	var headings = [].slice.call(documentRef.body.querySelectorAll('article h1, article h2, article h3, article h4, article h5, article h6'));
	if (headings.length < 2) { return };
	headings.forEach(function (heading, index) {
		var ref = heading.getAttribute( "id" );
		var link = documentRef.createElement( "a" );
		link.setAttribute( "href", "#"+ ref );
		link.textContent = heading.textContent;
		var li = documentRef.createElement( "li" );
		li.setAttribute( "class", heading.tagName.toLowerCase() );
		li.appendChild( link );
		toc.appendChild( li );
	});
}
</script></head><body><nav><a href="/?index">&#10087; index</a></nav>
<ul id="toc"></ul>
<article>
{{.Body}}
</article></body>
`

const extensions = parser.CommonExtensions | parser.AutoHeadingIDs ^ parser.MathJax

var rendererOpts = html.RendererOptions{Flags: html.CommonFlags}
var policy = bluemonday.UGCPolicy()

func containsDotDot(v string) bool {
	if !strings.Contains(v, "..") {
		return false
	}
	for _, ent := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }) {
		if ent == ".." {
			return true
		}
	}
	return false
}

const style = `body {
	font-family: Charter, Constantia, serif;
	font-size: 1rem;
	line-height: 170%;
	max-width: 45em;
	margin: auto;
	padding-right: 1em;
	padding-left: 1em;
	color: #333;
	background: white;
	text-rendering: optimizeLegibility;
}

@media only screen and (max-width: 480px) {
	body {
		font-size: 125%;
		text-rendering: auto;
	}
}

a {color: #a08941; text-decoration: none;}
a:hover {color: #c6b754; text-decoration: underline;}

h1 a, h2 a, h3 a, h4 a, h5 a {
	text-decoration: none;
	color: gray;
	break-after: avoid;
}
h1 a:hover, h2 a:hover, h3 a:hover, h4 a:hover, h5 a:hover {
	text-decoration: none;
	color: gray;
}
h1, h2, h3, h4, h5 {
	font-weight: bold;
	color: gray;
}

h1 {
	font-size: 150%;
}

h2 {
	font-size: 130%;
}

h3 {
	font-size: 110%;
}

h4, h5 {
	font-size: 100%;
	font-style: italic;
}

pre {
	background-color: rgba(200,200,200,0.2);
	color: #1111111;
	padding: 0.5em;
	overflow: auto;
}
code, pre {
	font-family: Consolas, "PT Mono", monospace;
}
pre { font-size: 90%; }

hr { border:none; text-align:center; color:gray; }
hr:after {
	content:"\2766";
	display:inline-block;
	font-size:1.5em;
}

dt code {
	font-weight: bold;
}
dd p {
	margin-top: 0;
}

blockquote {
	background-color: rgba(200,200,200,0.2);
	color: #1111111;
	padding: 0 0.5em;
}

img {display:block;margin:auto;max-width:100%}

table, td, th {
	border:thin solid lightgrey;
	border-collapse:collapse;
	vertical-aligh:middle;
}
td, th {padding:0.2em 0.5em}
tr:nth-child(even) {background-color: rgba(200,200,200,0.2)}

ul#toc:not(:empty):before { content:"Contents:"; font-weight:bold; color:gray }
ul#toc:not(:empty):after {
	content:"\2042";
	text-align:center;
	display:block;
	color:gray;
}
ul#toc {list-style: none;padding-left:0}
ul#toc li.h2 {padding-left:1em}
ul#toc li.h3 {padding-left:2em}
ul#toc li.h4 {padding-left:3em}
ul#toc li.h5 {padding-left:4em}
ul#toc li.h6 {padding-left:5em}

nav {
	font-size:90%;
	text-align:right;
	padding:.5em;
	border-bottom: 1px solid gray;
}

@media print {
	nav, ul#toc {display: none}
	pre {overflow-wrap:break-word; white-space:pre-wrap}
}`

//go:generate sh -c "go doc >README"
