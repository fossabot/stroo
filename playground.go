package stroo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/mux"
	"go/format"
	"golang.org/x/tools/go/packages"
	"log"
	"net/http"
	"os"
	"text/template"
)

const (
	playgroundVersion = "0.0.1"
	storageFolder     = "/server"
	indexHTML         = "/index.html"
	codeTextAreaHTML  = "/codetextarea.html"
	playgroundHTML    = "/playground.html"
	favico            = "/favicon.ico"
	exampleGo         = "/example-source.go"
	exampleTemplate   = "/example-template.tpl"
)

func exampleSourceHandler(workingDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, workingDir+storageFolder+exampleGo)
	}
}

func exampleTemplateHandler(workingDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, workingDir+storageFolder+exampleTemplate)
	}
}

func filesHandler(workingDir string) http.HandlerFunc {
	// prepare index.html
	index, err := template.ParseFiles(workingDir + storageFolder + indexHTML)
	if err != nil {
		log.Fatalf("Error parsing template : %v\n working dir is = %q", err, workingDir)
	}
	type pageinfo struct {
		Version string
	}
	info := pageinfo{
		Version: playgroundVersion,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case indexHTML, "/":
			if err := index.ExecuteTemplate(w, "index", info); err != nil {
				log.Printf("error producing index template : %v", err)
				return
			}
		case codeTextAreaHTML, playgroundHTML:
			http.ServeFile(w, r, workingDir+storageFolder+"/"+r.URL.Path)
		case favico:
			// just ignore it
		default:
			log.Printf("Requested UNKNOWN URL : %q", r.URL.Path)
		}
	})
}

type ErrorType int

const (
	Json           ErrorType = 1
	TemplaParse    ErrorType = 2
	BadTempProject ErrorType = 3
	PackaLoad      ErrorType = 4
	OnePackage     ErrorType = 5
	Packalyse      ErrorType = 6
	NoTypes        ErrorType = 7
	TemplExe       ErrorType = 8
	BadFormat      ErrorType = 9
)

type MalformedRequest struct {
	Status int
	Error  string
	Type   ErrorType
}

func respond(w http.ResponseWriter, data interface{}) {
	if err, ok := data.(error); ok {
		var (
			typedError         MalformedRequest
			malformedRequest   *MalformedRequest
			syntaxError        *json.SyntaxError
			unmarshalTypeError *json.UnmarshalTypeError
		)
		switch {
		case errors.As(err, &syntaxError):
		case errors.As(err, &unmarshalTypeError):
			typedError = MalformedRequest{
				Error:  err.Error(),
				Status: http.StatusBadRequest,
				Type:   Json,
			}
		case errors.As(err, &malformedRequest):
			typedError = *malformedRequest
		}
		data = typedError
		w.WriteHeader(typedError.Status)
		log.Printf("Error : %#v", typedError)
	} else {
		w.WriteHeader(http.StatusOK) // status is ok
	}
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(data)
	if err != nil {
		log.Printf("Error encoding data : %v", err)
	}
}

type previewRequest struct {
	Template      string
	Source        string
	SourceChanged bool
}

type previewResponse struct {
	Result string `json:"result"`
}

// TODO : allow multiple packages, separated by something (e.g. commented "---")
// TODO : provide multiple types of errors, so we can track line numbers and things
func strooHandler(command *Command) http.HandlerFunc {

	var cachedResult *Code

	return func(w http.ResponseWriter, r *http.Request) {
		var request previewRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			respond(w, err)
			return
		}
		// template is more likely to change : we're processing it first
		tmpTemplate, err := template.New("template").Funcs(DefaultFuncMap()).Parse(request.Template)
		if err != nil {
			respond(w, MalformedRequest{Error: err.Error(), Type: TemplaParse, Status: http.StatusNoContent})
			return
		}
		// we're using cached result, so we don't stress the disk for nothing
		if request.SourceChanged || cachedResult == nil {
			// prepare a temp project
			tempProj, err := CreateTempProj([]TemporaryPackage{{Name: "playground", Files: map[string]interface{}{"file.go": request.Source}}})
			if err != nil {
				respond(w, MalformedRequest{Error: err.Error(), Type: BadTempProject})
				return
			}
			// setup cleanup, so temporary files and folders gets deleted
			defer tempProj.Cleanup()

			tempProj.Config.Mode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedImports
			// load package using the old way
			thePackages, err := packages.Load(tempProj.Config, fmt.Sprintf("file=%s", tempProj.File("playground", "file.go")))
			if err != nil {
				respond(w, MalformedRequest{Error: err.Error(), Type: PackaLoad})
				return
			}
			if len(thePackages) != 1 {
				respond(w, MalformedRequest{Error: "expecting exactly one package", Type: OnePackage})
				return
			}
			// create a temporary command to analyse the loaded package
			tempCommand := NewCommand(DefaultAnalyzer())
			tempCommand.TestMode = command.TestMode
			if err := tempCommand.Analyse(thePackages[0]); err != nil {
				respond(w, MalformedRequest{Error: err.Error(), Type: Packalyse})
				return
			}
			// convention : by default, the upper most type struct is provided to the code builder
			var firstType *TypeInfo
			if len(tempCommand.Result.Types) >= 1 {
				firstType = tempCommand.Result.Types[0]
			} else {
				// TODO : allow no types selected - might just want to work with interfaces
				respond(w, MalformedRequest{Error: "no types found. please a a type", Type: NoTypes})
				return
			}
			// create code
			result := Code{
				PackageInfo: tempCommand.Result,
				CodeConfig:  tempCommand.CodeConfig,
				keeper:      make(map[string]interface{}),
				tmpl:        tmpTemplate,
				Main:        TypeWithRoot{T: firstType},
			}
			result.Main.D = &result
			cachedResult = &result
		}

		// finally, we're processing the template over the result
		var buf bytes.Buffer
		if err := tmpTemplate.Execute(&buf, cachedResult); err != nil {
			respond(w, MalformedRequest{Error: err.Error(), Type: TemplExe})
			return
		}

		formatted, err := format.Source(buf.Bytes())
		if err != nil {
			log.Printf("bad format error : %v\nGo source:\n%s\n", err, buf.String())
			respond(w, MalformedRequest{Error: err.Error(), Type: BadFormat})
			return
		}
		response := previewResponse{Result: string(formatted)}
		respond(w, response)
	}
}

func StartPlayground(command *Command) {
	log.Printf("Starting on http://localhost:8080\n")
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("could NOT obtain current workdir : %v", err)
	}
	router := mux.NewRouter()
	router.Handle("/example-source", exampleSourceHandler(wd)).Methods("GET")
	router.Handle("/example-template", exampleTemplateHandler(wd)).Methods("GET")
	router.NotFoundHandler = filesHandler(wd)
	router.HandleFunc("/stroo-it", strooHandler(command)).Methods("POST")
	if err := http.ListenAndServe("0.0.0.0:8080", router); err != nil {
		log.Fatalf("error while serving : %v", err)
	}
	log.Printf("Server started.")
}
