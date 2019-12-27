package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/badu/stroo/codescan"

	. "github.com/badu/stroo/stroo"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-openapi/swag"
)

var mAnalyzer = &codescan.Analyzer{
	Name: "inspect",
	Doc:  "AST traversal for later passes",
	Runnner: func(pass *codescan.Pass) (interface{}, error) {
		return codescan.NewInpector(pass.Files), nil
	},
	RunDespiteErrors: true,
	ResultType:       reflect.TypeOf(new(codescan.Inspector)),
}

func run(pass *codescan.Pass) (interface{}, error) {
	var (
		nodeFilter = []ast.Node{
			(*ast.FuncDecl)(nil),
			(*ast.FuncLit)(nil),
			(*ast.GenDecl)(nil),
		}
		err    error
		result = &PackageInfo{Name: pass.Pkg.Name(), StructDefs: make(map[string]*TypeInfo), PrintDebug: mAnalyzer.PrintDebug}
	)
	inspector, ok := pass.ResultOf[mAnalyzer].(*codescan.Inspector)
	if !ok {
		log.Fatalf("Inspector is not (*codescan.Inspector)")
	}
	result.TypesInfo = pass.TypesInfo // exposed just in case someone wants to get wild
	//	var sb strings.Builder
	//	pass.Debug(&sb)
	//	log.Println(sb.String())
	inspector.Do(nodeFilter, func(node ast.Node) {
		if err != nil {
			return // we have error for a previous step
		}
		switch nodeType := node.(type) {
		case *ast.FuncDecl:
			result.ReadFunctionInfo(nodeType)
		case *ast.GenDecl:
			switch nodeType.Tok {
			case token.TYPE:
				for _, spec := range nodeType.Specs {
					typeSpec := spec.(*ast.TypeSpec)
					switch unknownType := typeSpec.Type.(type) {
					case *ast.InterfaceType:
						// e.g. `type Intf interface{}`
						result.ReadInterfaceInfo(spec, nodeType.Doc)
					case *ast.ArrayType:
						// e.g. `type Array []string`
						if infoErr := result.ReadArrayInfo(spec.(*ast.TypeSpec), nodeType.Doc); infoErr != nil {
							err = infoErr
						}
						// e.g. `type Stru struct {}`
					case *ast.StructType:
						if infoErr := result.ReadStructInfo(spec.(*ast.TypeSpec), nodeType.Doc); infoErr != nil {
							err = infoErr
						}
					case *ast.Ident:
						// e.g. : `type String string`
						result.ReadIdent(unknownType, nil, nodeType.Doc)

					default:
						log.Printf("Have you modified the filter ? Unhandled : %#v\n", unknownType)
					}
				}
			case token.VAR, token.CONST:
				for _, spec := range nodeType.Specs {
					switch vl := spec.(type) {
					case *ast.ValueSpec:
						result.ReadVariablesInfo(spec, vl)
					}
				}
			}
		}
	})

	if err != nil {
		return nil, err
	}
	return result, result.PostProcess()
}

var (
	typeName     = flag.String("type", "", "type that should be processed e.g. SomeJsonPayload")
	outputFile   = flag.String("output", "", "name of the output file e.g. json_gen.go")
	templateFile = flag.String("template", "", "name of the template file e.g. ./../templates/")
	peerStruct   = flag.String("target", "", "name of the peer struct e.g. ./../testdata/pkg/model_b/SomeProtoBufPayload")
	testMode     = flag.Bool("testmode", false, "is in test mode : just display the result")
	debugPrint   = flag.Bool("debug", false, "print debugging info")
)

func loadTemplate(path string, fnMap template.FuncMap) (*template.Template, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("error : %v ; path = %q", err, path)
	}
	return template.Must(template.New(filepath.Base(path)).Funcs(fnMap).ParseFiles(path)), nil
}

func contains(args ...string) bool {
	who := args[0]
	for i := 1; i < len(args); i++ {
		if args[i] == who {
			return true
		}
	}
	return false
}

func empty(src string) bool {
	if src == "" {
		return true
	}
	return false
}

func lowerInitial(str string) string {
	for i, v := range str {
		return string(unicode.ToLower(v)) + str[i+1:]
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToTitle(r)) + s[n:]
}

func templateGoStr(input string) string {
	if len(input) > 0 && input[len(input)-1] == '\n' {
		input = input[0 : len(input)-1]
	}
	if strings.Contains(input, "`") {
		lines := strings.Split(input, "\n")
		for idx, line := range lines {
			lines[idx] = strconv.Quote(line + "\n")
		}
		return strings.Join(lines, " + \n")
	}
	return "`" + input + "`"
}

func isNil(value interface{}) bool {
	if value == nil {
		return true
	}
	return false
}

type Doc struct {
	Imports          []string
	GeneratedMethods []string
	PackageInfo      *PackageInfo
	CurrentType      *TypeInfo
	Main             TypeWithRoot
	SelectedType     string                 // from flags
	OutputFile       string                 // from flags
	TemplateFile     string                 // from flags
	PeerName         string                 // from flags
	TestMode         bool                   // from flags
	keeper           map[string]interface{} // template keeps data in here, key-value, as they need
}

type TypeWithRoot struct {
	T *TypeInfo
	D *Doc
}

func (d *Doc) SetSelectedTypeNotNil() bool {
	if d.CurrentType == nil {
		log.Println("Selected type is nil")
		return false
	}
	if *debugPrint {
		log.Println("SetSelectedTypeNotNil " + d.CurrentType.Kind)
	}
	return true
}

func (d *Doc) SetSelectedTypeInfo(newType *TypeInfo) *TypeInfo {
	if newType == nil {
		log.Println("error : new type is nil")
		return nil
	}
	d.SelectedType = newType.Kind
	found := false
	d.CurrentType, found = d.PackageInfo.StructDefs[newType.Kind]
	if !found {
		log.Printf("%q not found while setting selected type", newType.Kind)
	}
	if *debugPrint {
		log.Println("Select type " + d.CurrentType.Kind)
	}
	return d.CurrentType
}

func (d *Doc) SetSelectedType(newType string) string {
	d.SelectedType = newType
	found := false
	d.CurrentType, found = d.PackageInfo.StructDefs[newType]
	if !found {
		log.Printf("%q not found while setting selected type", newType)
		return ""
	}
	if *debugPrint {
		log.Println("SetSelectedType " + d.CurrentType.Kind)
	}
	return ""
}

func (d *Doc) GetStructByKey(key string) *TypeInfo {
	structInfo, ok := d.PackageInfo.StructDefs[key]
	if ok {
		return structInfo
	}
	return nil
}

// returns true if the key exist and will overwrite
func (d *Doc) Store(key string, value interface{}) bool {
	_, has := d.keeper[key]
	d.keeper[key] = value
	return has
}

func (d *Doc) Retrieve(key string) interface{} {
	value, _ := d.keeper[key]
	return value
}

func (d *Doc) HasInStore(key string) bool {
	_, has := d.keeper[key]
	if *debugPrint {
		log.Printf("Has in store %q = %t", key, has)
	}
	return has
}

func (d *Doc) Keeper() map[string]interface{} {
	return d.keeper
}

func (d *Doc) AddToImports(imp string) string {
	d.Imports = append(d.Imports, imp)
	return ""
}

func (d *Doc) AddToGeneratedMethods(methodName string) string {
	d.GeneratedMethods = append(d.GeneratedMethods, methodName)
	return ""
}

func (d *Doc) Header() string {
	flags := ""
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		flags += "-" + f.Name + "=" + f.Value.String() + " "
	})
	return fmt.Sprintf("// Generated on %v by Stroo [https://github.com/badu/stroo]\n"+
		"// Do NOT bother with altering it by hand : use the tool\n"+
		"// Arguments at the time of generation:\n//\t%s", time.Now().Format("Mon Jan 2 15:04:05"), flags)
}

func SortFields(fields Fields) bool {
	sort.Sort(fields)
	return true
}

func main() {
	analyzer := &codescan.Analyzer{
		Name:             "stroo",
		Doc:              "extracts declaration of a struct with it's methods",
		Flags:            flag.FlagSet{},
		Runnner:          run,
		RunDespiteErrors: true,
		Requires:         codescan.Analyzers{mAnalyzer},
		ResultType:       reflect.TypeOf(new(PackageInfo)),
	}

	log.SetFlags(0)
	log.SetPrefix(analyzer.Name + ": ")
	analyzers := codescan.Analyzers{analyzer}
	if err := analyzers.Validate(); err != nil {
		log.Fatal(err)
	}
	analyzers.ParseFlags()

	var (
		tmpl         *template.Template
		templatePath string
		err          error
	)
	templatePath, err = filepath.Abs(*templateFile)

	log.Printf("Processing type : %q - test mode : %t, printing debug : %t\n", *typeName, *testMode, *debugPrint)

	flag.Usage = func() {
		paras := strings.Split(analyzer.Doc, "\n\n")
		fmt.Fprintf(os.Stderr, "%s: %s\n\n", analyzer.Name, paras[0])
		fmt.Fprintf(os.Stderr, "Usage: %s [-flag] [package]\n\n", analyzer.Name)
		if len(paras) > 1 {
			fmt.Fprintln(os.Stderr, strings.Join(paras[1:], "\n\n"))
		}
		fmt.Fprintf(os.Stderr, "\nFlags:")
		flag.PrintDefaults()
	}

	args := flag.Args()
	if *templateFile == "" {
		log.Fatal("Error : you have to provide a template file (with relative path)")
	}
	if *typeName == "" {
		log.Fatal("Error : you have to provide a main type to be used in the template")
	}
	if !*testMode && *outputFile == "" {
		log.Fatal("Error : you have to specify the Go file which will be produced")
	}
	mAnalyzer.PrintDebug = *debugPrint

	initial, err := codescan.Load(args)
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}

	roots := initial.Analyze(analyzers)

	results, exitCode := roots.GatherResults()
	if len(results) == 1 {
		packageInfo, ok := results[0].(*PackageInfo)
		if !ok {
			log.Fatalf("Error : interface not *PackageInfo")
		}

		wkdir, _ := os.Getwd()
		originalWkDir := wkdir
		goPath := os.Getenv(codescan.GOPATH)
		goPathParts := strings.Split(goPath, ":")
		for _, part := range goPathParts {
			wkdir = strings.Replace(wkdir, part, "", -1)
		}
		if strings.HasPrefix(wkdir, codescan.SrcFPath) {
			wkdir = wkdir[5:]
		}
		doc := Doc{
			PackageInfo:  packageInfo,
			SelectedType: *typeName,
			OutputFile:   *outputFile,
			PeerName:     *peerStruct,
			TemplateFile: *templateFile,
			TestMode:     *testMode,
			keeper:       make(map[string]interface{}),
		}
		doc.Main = TypeWithRoot{D: &doc, T: packageInfo.GetStructByKey(*typeName)}

		tmpl, err = loadTemplate(templatePath, template.FuncMap{
			"in":            contains,
			"empty":         empty,
			"nil":           isNil,
			"lowerInitial":  lowerInitial,
			"capitalize":    capitalize,
			"templateGoStr": templateGoStr,
			"trim":          strings.TrimSpace,
			"hasPrefix":     strings.HasPrefix,
			"toJsonName":    swag.ToJSONName, // TODO : import all, but make it field functionality
			"sort":          SortFields,      // TODO : test sort fields (fields implements the interface)
			"dump": func(a ...interface{}) string {
				return spew.Sdump(a...)
			},
			"include": func(name string, data *TypeInfo) string {
				var buf strings.Builder
				err := tmpl.ExecuteTemplate(&buf, name, TypeWithRoot{D: &doc, T: data})
				if err != nil {
					log.Printf("Include Error : %v", err)
				}
				return buf.String()
			},
			"includeAndStore": func(name string, data *TypeInfo, storeName string) bool {
				var buf strings.Builder
				err := tmpl.ExecuteTemplate(&buf, name, TypeWithRoot{D: &doc, T: data})
				if err != nil {
					log.Printf("Include Error : %v", err)
					return false
				}
				result := buf.String()
				doc.keeper[storeName] = result
				if *debugPrint {
					log.Printf("%q stored.", storeName)
				}
				return true
			},
			"includeAndStoreArray": func(name string, data *FieldInfo, storeName string) bool {
				var buf strings.Builder
				if *debugPrint {
					log.Printf("DATA : %#v", data)
				}
				err := tmpl.ExecuteTemplate(&buf, name, TypeWithRoot{D: &doc, T: &TypeInfo{Name: data.Name, Kind: data.Name, Fields: Fields{data}, IsArray: true}})
				if err != nil {
					log.Printf("Include Error : %v", err)
					return false
				}
				result := buf.String()
				doc.keeper[storeName] = result
				if *debugPrint {
					log.Printf("%q stored.", storeName)
				}
				return true
			},
			"concat": func(a, b string) string {
				return a + b
			},
		})
		if err != nil {
			log.Fatal(err)
		}

		buf := bytes.Buffer{}
		if err := tmpl.Execute(&buf, &doc); err != nil {
			log.Fatalf("failed to parse template %s: %s\nPartial result:\n%s", *templateFile, err, buf.String())
		}
		// forced add header
		var src []byte
		src = append(src, doc.Header()...)
		src = append(src, buf.Bytes()...)
		formatted, err := format.Source(src)
		if err != nil {
			log.Fatalf("go/format: %s\nResult:\n%s", err, src)
		} else if !*testMode {
			/**
			if _, err := os.Stat(*outputFile); !os.IsNotExist(err) {
				log.Fatalf("destination exists = %q", *outputFile)
			}
			**/
			log.Printf("Creating %s/%s\n", originalWkDir, *outputFile)
			file, err := os.Create(originalWkDir + "/" + *outputFile)
			if err != nil {
				log.Fatalf("Error creating output: %v", err)
			}
			// go ahead and write the file
			if _, err := file.Write(formatted); err != nil {
				log.Fatalf("error writing : %v", err)
			}
		} else {
			log.Println(string(formatted))
		}
	}
	os.Exit(exitCode)
}
