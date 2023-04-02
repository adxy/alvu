package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/barelyhuman/go/env"
	ghttp "github.com/cjoudrey/gluahttp"

	"github.com/barelyhuman/go/color"
	cp "github.com/otiai10/copy"

	stringsLib "github.com/vadv/gopher-lua-libs/strings"

	yamlLib "github.com/vadv/gopher-lua-libs/yaml"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"

	highlighting "github.com/yuin/goldmark-highlighting"

	lua "github.com/yuin/gopher-lua"
	"gopkg.in/yaml.v3"

	luaAlvu "codeberg.org/reaper/alvu/lua/alvu"
	luajson "layeh.com/gopher-json"
)

const logPrefix = "[alvu] "

var mdProcessor goldmark.Markdown
var baseurl string
var basePath string
var outPath string
var hardWraps bool
var hookCollection HookCollection

var reservedFiles []string = []string{"_head.html", "_tail.html", "_layout.html"}

type SiteMeta struct {
	BaseURL string
}

type PageRenderData struct {
	Meta   SiteMeta
	Data   map[string]interface{}
	Extras map[string]interface{}
}

type LayoutRenderData struct {
	PageRenderData
	Content template.HTML
}

func main() {
	onDebug(func() {
		debugInfo("Before Exec")
		memuse()
	})

	basePathFlag := flag.String("path", ".", "`DIR` to search for the needed folders in")
	outPathFlag := flag.String("out", "./dist", "`DIR` to output the compiled files to")
	baseurlFlag := flag.String("baseurl", "/", "`URL` to be used as the root of the project")
	hooksPathFlag := flag.String("hooks", "./hooks", "`DIR` that contains hooks for the content")
	enableHighlightingFlag := flag.Bool("highlight", false, "enable highlighting for markdown files")
	highlightThemeFlag := flag.String("highlight-theme", "bw", "`THEME` to use for highlighting (supports most themes from pygments)")
	serveFlag := flag.Bool("serve", false, "start a local server")
	hardWrapsFlag := flag.Bool("hard-wrap", true, "enable hard wrapping of elements with `<br>`")
	portFlag := flag.String("port", "3000", "`PORT` to start the server on")

	flag.Parse()

	baseurl = *baseurlFlag
	basePath = path.Join(*basePathFlag)
	pagesPath := path.Join(*basePathFlag, "pages")
	publicPath := path.Join(*basePathFlag, "public")
	headFilePath := path.Join(pagesPath, "_head.html")
	baseFilePath := path.Join(pagesPath, "_layout.html")
	tailFilePath := path.Join(pagesPath, "_tail.html")
	outPath = path.Join(*outPathFlag)
	hooksPath := path.Join(*basePathFlag, *hooksPathFlag)
	hardWraps = *hardWrapsFlag

	headTailDeprecationWarning := color.ColorString{}
	headTailDeprecationWarning.Yellow(logPrefix).Yellow("[WARN] use of _tail.html and _head.html is deprecated, please use _layout.html instead")

	onDebug(func() {
		debugInfo("Opening _head")
		memuse()
	})
	headFileFd, err := os.Open(headFilePath)
	if err != nil {
		if err == fs.ErrNotExist {
			log.Println("no _head.html found,skipping")
		}
	} else {
		fmt.Println(headTailDeprecationWarning.String())
	}

	onDebug(func() {
		debugInfo("Opening _layout")
		memuse()
	})
	baseFileFd, err := os.Open(baseFilePath)
	if err != nil {
		if err == fs.ErrNotExist {
			log.Println("no _layout.html found,skipping")
		}
	}

	onDebug(func() {
		debugInfo("Opening _tail")
		memuse()
	})
	tailFileFd, err := os.Open(tailFilePath)
	if err != nil {
		if err == fs.ErrNotExist {
			log.Println("no _tail.html found, skipping")
		}
	} else {
		fmt.Println(headTailDeprecationWarning.String())
	}

	onDebug(func() {
		debugInfo("Before copying files")
		memuse()
	})
	// copy public to out
	_, err = os.Stat(publicPath)
	if err == nil {
		err = cp.Copy(publicPath, outPath)
		if err != nil {
			bail(err)
		}
	}
	onDebug(func() {
		debugInfo("After copying files")
		memuse()
	})

	onDebug(func() {
		debugInfo("Reading hook and to process files")
		memuse()
	})
	CollectHooks(basePath, hooksPath)
	toProcess := CollectFilesToProcess(pagesPath)
	onDebug(func() {
		log.Println("printing files to process")
		log.Println(toProcess)
	})

	initMDProcessor(*enableHighlightingFlag, *highlightThemeFlag)

	onDebug(func() {
		debugInfo("Running all OnStart hooks")
		memuse()
	})
	hookCollection.RunAll("OnStart")

	prefixSlashPath := regexp.MustCompile(`^\/`)

	onDebug(func() {
		debugInfo("Processing Files")
		memuse()
	})
	for _, toProcessItem := range toProcess {
		fileName := strings.Replace(toProcessItem, pagesPath, "", 1)
		fileName = prefixSlashPath.ReplaceAllString(fileName, "")
		destFilePath := strings.Replace(toProcessItem, pagesPath, outPath, 1)

		alvuFile := &AlvuFile{
			lock:         &sync.Mutex{},
			sourcePath:   toProcessItem,
			destPath:     destFilePath,
			name:         fileName,
			headFile:     headFileFd,
			tailFile:     tailFileFd,
			baseTemplate: baseFileFd,
			data:         map[string]interface{}{},
			extras:       map[string]interface{}{},
		}

		bail(alvuFile.ReadFile())
		bail(alvuFile.ParseMeta())

		// If no hooks are present just process the files
		if len(hookCollection) == 0 {
			alvuFile.ProcessFile(nil)
		}

		for _, hook := range hookCollection {

			isForSpecificFile := hook.state.GetGlobal("ForFile")

			if isForSpecificFile != lua.LNil {
				if alvuFile.name == isForSpecificFile.String() {
					alvuFile.ProcessFile(hook.state)
				} else {
					bail(alvuFile.ProcessFile(nil))
				}
			} else {
				bail(alvuFile.ProcessFile(hook.state))
			}
		}
		alvuFile.FlushFile()
	}
	onDebug(func() {
		debugInfo("Run all OnFinish Hooks")
		memuse()
	})
	// right before completion run all hooks again but for the onFinish
	hookCollection.RunAll("OnFinish")
	hookCollection.Shutdown()

	onDebug(func() {
		runtime.GC()
		debugInfo("On Completions")
		memuse()
	})

	cs := &color.ColorString{}
	fmt.Println(cs.Blue(logPrefix).Green("Compiled ").Cyan("\"" + basePath + "\"").Green(" to ").Cyan("\"" + outPath + "\"").String())

	if *serveFlag {
		runServer(*portFlag)
	}

}

func runServer(port string) {

	normalizedPort := port

	if !strings.HasPrefix(normalizedPort, ":") {
		normalizedPort = ":" + normalizedPort
	}

	cs := &color.ColorString{}
	cs.Blue(logPrefix).Green("Serving on").Reset(" ").Cyan(normalizedPort)
	fmt.Println(cs.String())
	err := http.ListenAndServe(normalizedPort, http.HandlerFunc(ServeHandler))

	if strings.Contains(err.Error(), "address already in use") {
		bail(errors.New("port already in use, use another port with the `-port` flag instead"))
	}

}

func CollectFilesToProcess(basepath string) []string {
	files := []string{}

	pathstoprocess, err := os.ReadDir(basepath)
	if err != nil {
		panic(err)
	}

	for _, pathInfo := range pathstoprocess {
		_path := path.Join(basepath, pathInfo.Name())

		if Contains(reservedFiles, pathInfo.Name()) {
			continue
		}

		if pathInfo.IsDir() {
			files = append(files, CollectFilesToProcess(_path)...)
		} else {
			files = append(files, _path)
		}

	}

	return files
}

func CollectHooks(basePath, hooksBasePath string) {
	if _, err := os.Stat(hooksBasePath); err != nil {
		return
	}
	pathsToProcess, err := os.ReadDir(hooksBasePath)
	if err != nil {
		panic(err)
	}

	for _, pathInfo := range pathsToProcess {
		if !strings.HasSuffix(pathInfo.Name(), ".lua") {
			continue
		}
		hook := NewHook()
		hookPath := path.Join(hooksBasePath, pathInfo.Name())
		if err := hook.DoFile(hookPath); err != nil {
			panic(err)
		}
		hookCollection = append(hookCollection, &Hook{
			path:  hookPath,
			state: hook,
		})
	}

}

func initMDProcessor(highlight bool, theme string) {

	rendererOptions := []renderer.Option{
		html.WithXHTML(),
		html.WithUnsafe(),
	}

	if hardWraps {
		rendererOptions = append(rendererOptions, html.WithHardWraps())
	}
	gmPlugins := []goldmark.Option{
		goldmark.WithExtensions(extension.GFM, extension.Footnote),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			rendererOptions...,
		),
	}

	if highlight {
		gmPlugins = append(gmPlugins, goldmark.WithExtensions(
			highlighting.NewHighlighting(
				highlighting.WithStyle(theme),
			),
		))
	}

	mdProcessor = goldmark.New(gmPlugins...)
}

type Hook struct {
	path  string
	state *lua.LState
}

type HookCollection []*Hook

func (hc HookCollection) Shutdown() {
	for _, hook := range hc {
		hook.state.Close()
	}
}

func (hc HookCollection) RunAll(funcName string) {
	for _, hook := range hc {
		hookFunc := hook.state.GetGlobal(funcName)

		if hookFunc == lua.LNil {
			continue
		}

		if err := hook.state.CallByParam(lua.P{
			Fn:      hookFunc,
			NRet:    0,
			Protect: true,
		}); err != nil {
			bail(err)
		}
	}
}

type AlvuFile struct {
	lock             *sync.Mutex
	name             string
	sourcePath       string
	destPath         string
	meta             map[string]interface{}
	content          []byte
	writeableContent []byte
	headFile         *os.File
	tailFile         *os.File
	baseTemplate     *os.File
	targetName       []byte
	data             map[string]interface{}
	extras           map[string]interface{}
}

func (a *AlvuFile) ReadFile() error {
	filecontent, err := os.ReadFile(a.sourcePath)
	if err != nil {
		return fmt.Errorf("error reading file, error: %v", err)
	}
	a.content = filecontent
	return nil
}

func (a *AlvuFile) ParseMeta() error {
	sep := []byte("---")
	if !bytes.HasPrefix(a.content, sep) {
		a.writeableContent = a.content
		return nil
	}

	metaParts := bytes.SplitN(a.content, sep, 3)

	var meta map[string]interface{}
	err := yaml.Unmarshal([]byte(metaParts[1]), &meta)
	if err != nil {
		return err
	}

	a.meta = meta
	a.writeableContent = []byte(metaParts[2])

	return nil
}

func (a *AlvuFile) ProcessFile(hook *lua.LState) error {
	// pre process hook => should return back json with `content` and `data`
	a.lock.Lock()
	defer a.lock.Unlock()

	a.targetName = regexp.MustCompile(`\.md$`).ReplaceAll([]byte(a.name), []byte(".html"))
	onDebug(func() {
		debugInfo(a.name + " will be changed to " + string(a.targetName))
	})

	buf := bytes.NewBuffer([]byte(""))
	mdToHTML := ""

	if filepath.Ext(a.name) == ".md" {
		newName := strings.Replace(a.name, filepath.Ext(a.name), ".html", 1)
		a.targetName = []byte(newName)
		mdProcessor.Convert(a.writeableContent, buf)
		mdToHTML = buf.String()
	}

	if hook == nil {
		return nil
	}

	hookInput := struct {
		Name             string                 `json:"name"`
		SourcePath       string                 `json:"source_path"`
		DestPath         string                 `json:"dest_path"`
		Meta             map[string]interface{} `json:"meta"`
		WriteableContent string                 `json:"content"`
		HTMLContent      string                 `json:"html"`
	}{
		Name:             string(a.targetName),
		SourcePath:       a.sourcePath,
		DestPath:         a.destPath,
		Meta:             a.meta,
		WriteableContent: string(a.writeableContent),
		HTMLContent:      mdToHTML,
	}

	hookJsonInput, err := json.Marshal(hookInput)
	bail(err)

	if err := hook.CallByParam(lua.P{
		Fn:      hook.GetGlobal("Writer"),
		NRet:    1,
		Protect: true,
	}, lua.LString(hookJsonInput)); err != nil {
		panic(err)
	}

	ret := hook.Get(-1)

	var fromPlug map[string]interface{}

	err = json.Unmarshal([]byte(ret.String()), &fromPlug)
	bail(err)

	if fromPlug["content"] != nil {
		stringVal := fmt.Sprintf("%s", fromPlug["content"])
		a.writeableContent = []byte(stringVal)
	}

	if fromPlug["name"] != nil {
		a.targetName = []byte(fmt.Sprintf("%v", fromPlug["name"]))
	}

	if fromPlug["data"] != nil {
		a.data = mergeMapWithCheck(a.data, fromPlug["data"])
	}

	if fromPlug["extras"] != nil {
		a.extras = mergeMapWithCheck(a.extras, fromPlug["extras"])
	}

	hook.Pop(1)
	return nil
}

func (a *AlvuFile) FlushFile() {
	destFolder := filepath.Dir(a.destPath)
	os.MkdirAll(destFolder, os.ModePerm)

	targetFile := strings.Replace(path.Join(a.destPath), a.name, string(a.targetName), 1)
	onDebug(func() {
		debugInfo("flushing for file: " + a.name + string(a.targetName))
		debugInfo("flusing file: " + targetFile)
	})

	f, err := os.Create(targetFile)
	bail(err)
	defer f.Sync()

	writeHeadTail := false

	if a.baseTemplate == nil && (filepath.Ext(a.sourcePath) == ".md" || filepath.Ext(a.sourcePath) == "html") {
		writeHeadTail = true
	}

	if writeHeadTail && a.headFile != nil {
		shouldCopyContentsWithReset(a.headFile, f)
	}

	renderData := PageRenderData{
		Meta: SiteMeta{
			BaseURL: baseurl,
		},
		Data:   a.data,
		Extras: a.extras,
	}

	// Run the Markdown file through the conversion
	// process to be able to use template variables in
	// the markdown instead of writing them in
	// raw HTML
	var preConvertHTML bytes.Buffer
	preConvertTmpl := template.New("temporary_pre_template")
	preConvertTmpl.Parse(string(a.writeableContent))
	err = preConvertTmpl.Execute(&preConvertHTML, renderData)
	bail(err)

	var toHtml bytes.Buffer
	err = mdProcessor.Convert(preConvertHTML.Bytes(), &toHtml)
	bail(err)

	layoutData := LayoutRenderData{
		PageRenderData: renderData,
		Content:        template.HTML(toHtml.Bytes()),
	}

	// If a layout file was found
	// write the converted html content into the
	// layout template file
	if a.baseTemplate != nil {
		layout := template.New("layout")
		layoutTemplateData := string(readFileToBytes(a.baseTemplate))
		toHtml.Reset()
		layout.Parse(layoutTemplateData)
		layout.Execute(&toHtml, layoutData)
	}

	io.Copy(
		f, &toHtml,
	)

	if writeHeadTail && a.tailFile != nil && a.baseTemplate == nil {
		shouldCopyContentsWithReset(a.tailFile, f)
	}

	data, err := os.ReadFile(targetFile)
	bail(err)

	onDebug(func() {
		debugInfo("template path: %v", a.sourcePath)
	})

	t := template.New(path.Join(a.sourcePath))
	t.Parse(string(data))

	f.Seek(0, 0)

	err = t.Execute(f, renderData)
	bail(err)
}

func NewHook() *lua.LState {
	lState := lua.NewState()
	luaAlvu.Preload(lState)
	luajson.Preload(lState)
	yamlLib.Preload(lState)
	stringsLib.Preload(lState)
	lState.PreloadModule("http", ghttp.NewHttpModule(&http.Client{}).Loader)
	if basePath == "." {
		lState.SetGlobal("workingdir", lua.LString(""))
	} else {
		lState.SetGlobal("workingdir", lua.LString(basePath))
	}
	return lState
}

// UTILS
func memuse() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("heap: %v MiB\n", bytesToMB(m.HeapAlloc))
}

func bytesToMB(inBytes uint64) uint64 {
	return inBytes / 1024 / 1024
}

func bail(err error) {
	if err == nil {
		return
	}
	cs := &color.ColorString{}
	fmt.Fprintln(os.Stderr, cs.Red(logPrefix).Red(": "+err.Error()).String())
	panic("")
}

func debugInfo(msg string, a ...any) {
	cs := &color.ColorString{}
	prefix := logPrefix
	baseMessage := cs.Reset("").Yellow(prefix).Reset(" ").Gray(msg).String()
	fmt.Fprintf(os.Stdout, baseMessage+" \n", a...)
}

func showDebug() bool {
	showInfo := env.Get("DEBUG_ALVU", "")
	return len(showInfo) != 0
}

func onDebug(fn func()) {
	if !showDebug() {
		return
	}

	fn()
}

func mergeMapWithCheck(maps ...any) (source map[string]interface{}) {
	source = map[string]interface{}{}
	for _, toCheck := range maps {
		if pairs, ok := toCheck.(map[string]interface{}); ok {
			for k, v := range pairs {
				source[k] = v
			}
		}
	}
	return source
}

func readFileToBytes(fd *os.File) []byte {
	buf := &bytes.Buffer{}
	fd.Seek(0, 0)
	_, err := io.Copy(buf, fd)
	bail(err)
	return buf.Bytes()
}

func shouldCopyContentsWithReset(src *os.File, target *os.File) {
	src.Seek(0, 0)
	_, err := io.Copy(target, src)
	bail(err)
}

func ServeHandler(rw http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	if path == "/" {
		path = filepath.Join(outPath, "index.html")
		http.ServeFile(rw, req, path)
		return
	}

	// check if the requested file already exists
	file := filepath.Join(outPath, path)
	info, err := os.Stat(file)

	// if not, check if it's a directory
	// and if it's a directory, we look for
	// a index.html inside the directory to return instead
	if err == nil {
		if info.Mode().IsDir() {
			file = filepath.Join(outPath, path, "index.html")
			_, err := os.Stat(file)
			if err != nil {
				notFoundHandler(rw, req)
				return
			}
		}

		http.ServeFile(rw, req, file)
		return
	}

	// if neither a directory or file was found
	// try a secondary case where the file might be missing
	// a `.html` extension for cleaner url so append a .html
	// to look for the file.
	if err != nil {
		file := filepath.Join(outPath, normalizeFilePath(path))
		_, err := os.Stat(file)

		if err != nil {
			notFoundHandler(rw, req)
			return
		}

		http.ServeFile(rw, req, file)
		return
	}

	notFoundHandler(rw, req)
}

func normalizeFilePath(path string) string {
	if strings.HasSuffix(path, ".html") {
		return path
	}
	return path + ".html"
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, "404, Page not found....")
}

func Contains(collection []string, item string) bool {
	for _, x := range collection {
		if item == x {
			return true
		}
	}
	return false
}
