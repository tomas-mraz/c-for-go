package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tomas-mraz/c-for-go/generator"
	"github.com/tomas-mraz/c-for-go/parser"
	"github.com/tomas-mraz/c-for-go/translator"
	"github.com/xlab/pkgconfig/pkg"
	"golang.org/x/tools/imports"
	"gopkg.in/yaml.v3"
	"modernc.org/cc/v4"
)

type Buf int

const (
	BufDoc Buf = iota
	BufConst
	BufTypes
	BufUnions
	BufHelpers
	BufMain
)

var goBufferNames = map[Buf]string{
	BufDoc:     "doc",
	BufConst:   "const",
	BufTypes:   "types",
	BufUnions:  "unions",
	BufHelpers: "cgo_helpers",
}

type Process struct {
	cfg              ProcessConfig
	gen              *generator.Generator
	genSync          sync.WaitGroup
	goBuffers        map[Buf]*bytes.Buffer
	chHelpersBuf     *bytes.Buffer
	ccHelpersBuf     *bytes.Buffer
	outputPath       string
	platformBuffers  map[string]*bytes.Buffer // platform output → combined Go buffer
	platformHelpers  map[string]*bytes.Buffer // platform output → Go helpers buffer
}

type ProcessConfig struct {
	Generator  *generator.Config  `yaml:"GENERATOR"`
	Translator *translator.Config `yaml:"TRANSLATOR"`
	Parser     *parser.Config     `yaml:"PARSER"`
}

func NewProcess(configPath, outputPath string) (*Process, error) {
	cfgData, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg ProcessConfig
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		return nil, err
	}
	if cfg.Generator != nil {
		paths := includePathsFromPkgConfig(cfg.Generator.PkgConfigOpts)
		if cfg.Parser == nil {
			cfg.Parser = &parser.Config{}
		}
		cfg.Parser.CCDefs = *ccDefs
		cfg.Parser.CCIncl = *ccIncl
		cfg.Parser.IncludePaths = append(cfg.Parser.IncludePaths, paths...)
		cfg.Parser.IncludePaths = append(cfg.Parser.IncludePaths, filepath.Dir(configPath))
		cfg.Parser.DefineLocations = extractParserDefineLocations(cfgData, filepath.Base(configPath))
	} else {
		return nil, errors.New("process: generator config was not specified")
	}

	// parse the headers
	unit, err := parser.ParseWith(cfg.Parser)
	if err != nil {
		return nil, err
	}

	if cfg.Translator == nil {
		cfg.Translator = &translator.Config{}
	}
	cfg.Translator.IgnoredFiles = cfg.Parser.IgnoredPaths
	cfg.Translator.LongIs64Bit = unit.ABI.Types[cc.Long].Size == 8
	// learn the model
	tl, err := translator.New(cfg.Translator)
	if err != nil {
		return nil, err
	}
	tl.Learn(unit)

	// begin generation
	pkg := filepath.Base(cfg.Generator.PackageName)
	gen, err := generator.New(pkg, cfg.Generator, tl)
	if err != nil {
		return nil, err
	}
	gen.SetMaxMemory(generator.NewMemSpec(*maxMem))

	if *nostamp {
		gen.DisableTimestamps()
	}
	c := &Process{
		cfg:             cfg,
		gen:             gen,
		goBuffers:       make(map[Buf]*bytes.Buffer),
		chHelpersBuf:    new(bytes.Buffer),
		ccHelpersBuf:    new(bytes.Buffer),
		outputPath:      outputPath,
		platformBuffers: make(map[string]*bytes.Buffer),
		platformHelpers: make(map[string]*bytes.Buffer),
	}
	c.goBuffers[BufMain] = new(bytes.Buffer)
	for opt := range goBufferNames {
		c.goBuffers[opt] = new(bytes.Buffer)
	}
	// Create platform-specific buffers
	for _, pf := range cfg.Generator.PlatformFiles {
		c.platformBuffers[pf.Output] = new(bytes.Buffer)
		c.platformHelpers[pf.Output] = new(bytes.Buffer)
	}
	goHelpersBuf := c.goBuffers[BufHelpers]
	platformHelperWriters := make(map[string]io.Writer, len(c.platformHelpers))
	for name, buf := range c.platformHelpers {
		platformHelperWriters[name] = buf
	}
	go func() {
		c.genSync.Add(1)
		c.gen.MonitorAndWriteHelpers(goHelpersBuf, c.chHelpersBuf, c.ccHelpersBuf, platformHelperWriters)
		c.genSync.Done()
	}()
	return c, nil
}

func extractParserDefineLocations(cfgData []byte, configFile string) map[string]parser.DefineLocation {
	var root yaml.Node
	if err := yaml.Unmarshal(cfgData, &root); err != nil {
		return nil
	}
	if len(root.Content) == 0 {
		return nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil
	}
	parserNode := getMapValue(top, "PARSER")
	if parserNode == nil || parserNode.Kind != yaml.MappingNode {
		return nil
	}
	definesNode := getMapValue(parserNode, "Defines")
	if definesNode == nil || definesNode.Kind != yaml.MappingNode {
		return nil
	}
	locations := make(map[string]parser.DefineLocation, len(definesNode.Content)/2)
	for i := 0; i+1 < len(definesNode.Content); i += 2 {
		key := definesNode.Content[i]
		locations[key.Value] = parser.DefineLocation{
			File: configFile,
			Line: key.Line,
		}
	}
	return locations
}

func getMapValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func (c *Process) Generate(noCGO bool) {
	// Build platform writer maps for types and declarations
	platformWriters := make(map[string]io.Writer, len(c.platformBuffers))
	for name, buf := range c.platformBuffers {
		platformWriters[name] = buf
	}

	main := c.goBuffers[BufMain]
	if wr, ok := c.goBuffers[BufDoc]; ok {
		if !c.gen.WriteDoc(wr) {
			c.goBuffers[BufDoc] = nil
		}
		c.gen.WritePackageHeader(main)
	} else {
		c.gen.WriteDoc(main)
	}
	if !noCGO {
		c.gen.WriteIncludes(main)
	}
	if wr, ok := c.goBuffers[BufConst]; ok {
		c.gen.WritePackageHeader(wr)
		if !noCGO {
			c.gen.WriteIncludes(wr)
		}
		if n := c.gen.WriteConst(wr); n == 0 {
			c.goBuffers[BufConst] = nil
		}
	} else {
		c.gen.WriteConst(main)
	}
	if wr, ok := c.goBuffers[BufTypes]; ok {
		c.gen.WritePackageHeader(wr)
		if !noCGO {
			c.gen.WriteIncludes(wr)
		}
		if n := c.gen.WriteTypedefs(wr, platformWriters); n == 0 {
			c.goBuffers[BufTypes] = nil
		}
	} else {
		c.gen.WriteTypedefs(main, platformWriters)
	}
	if !noCGO {
		if wr, ok := c.goBuffers[BufUnions]; ok {
			c.gen.WritePackageHeader(wr)
			c.gen.WriteIncludes(wr)
			if n := c.gen.WriteUnions(wr); n == 0 {
				c.goBuffers[BufUnions] = nil
			}
		} else {
			c.gen.WriteUnions(main)
		}
		c.gen.WriteDeclares(main, platformWriters)
	}
}

func (c *Process) Flush(noCGO bool) error {
	c.gen.Close()
	c.genSync.Wait()
	filePrefix := "."
	if c.outputPath != "" {
		filePrefix = filepath.Join(c.outputPath, c.cfg.Generator.PackageName)
	}
	if err := os.MkdirAll(filePrefix, 0755); err != nil {
		return err
	}
	createCHFile := func(name string) (*os.File, error) {
		return os.Create(filepath.Join(filePrefix, fmt.Sprintf("%s.h", name)))
	}
	createCCFile := func(name string) (*os.File, error) {
		return os.Create(filepath.Join(filePrefix, fmt.Sprintf("%s.c", name)))
	}
	createGoFile := func(name string) (*os.File, error) {
		return os.Create(filepath.Join(filePrefix, fmt.Sprintf("%s.go", name)))
	}
	writeGoFile := func(opt Buf, name string) error {
		if buf := c.goBuffers[opt]; buf != nil && buf.Len() > 0 {
			if f, err := createGoFile(name); err == nil {
				if err := flushBufferToFile(buf.Bytes(), f, true); err != nil {
					f.Close()
					return err
				}
				f.Close()
			} else {
				return err
			}
		}
		return nil
	}
	writeCHFile := func(buf *bytes.Buffer, name string) error {
		if f, err := createCHFile(name); err != nil {
			return err
		} else if err := flushBufferToFile(buf.Bytes(), f, false); err != nil {
			f.Close()
			return err
		} else {
			return f.Close()
		}
	}
	writeCCFile := func(buf *bytes.Buffer, name string) error {
		if f, err := createCCFile(name); err != nil {
			return err
		} else if err := flushBufferToFile(buf.Bytes(), f, false); err != nil {
			f.Close()
			return err
		} else {
			return f.Close()
		}
	}

	if !noCGO {
		pkg := filepath.Base(c.cfg.Generator.PackageName)
		if err := writeGoFile(BufMain, pkg); err != nil {
			return err
		}
	}
	for opt, name := range goBufferNames {
		if err := writeGoFile(opt, name); err != nil {
			return err
		}
	}
	if noCGO {
		return nil
	}
	if c.chHelpersBuf.Len() > 0 {
		if err := writeCHFile(c.chHelpersBuf, "cgo_helpers"); err != nil {
			return err
		}
	}
	if c.ccHelpersBuf.Len() > 0 {
		if err := writeCCFile(c.ccHelpersBuf, "cgo_helpers"); err != nil {
			return err
		}
	}

	// Write platform-specific files
	for _, pf := range c.cfg.Generator.PlatformFiles {
		typeBuf := c.platformBuffers[pf.Output]
		helperBuf := c.platformHelpers[pf.Output]
		if (typeBuf == nil || typeBuf.Len() == 0) && (helperBuf == nil || helperBuf.Len() == 0) {
			continue
		}
		combined := new(bytes.Buffer)
		// Write build tag
		if pf.BuildTag != "" {
			fmt.Fprintf(combined, "//go:build %s\n\n", pf.BuildTag)
		}
		// Write package header and includes with extra preamble
		c.gen.WritePackageHeader(combined)
		c.gen.WritePlatformIncludes(combined, pf.ExtraPreamble)
		// Write types and declarations
		if typeBuf != nil && typeBuf.Len() > 0 {
			combined.Write(typeBuf.Bytes())
		}
		// Write helpers
		if helperBuf != nil && helperBuf.Len() > 0 {
			combined.Write(helperBuf.Bytes())
		}
		if f, err := createGoFile(pf.Output); err == nil {
			if err := flushBufferToFile(combined.Bytes(), f, true); err != nil {
				f.Close()
				return err
			}
			f.Close()
		} else {
			return err
		}
	}

	return nil
}

func flushBufferToFile(buf []byte, f *os.File, fmt bool) error {
	if fmt {
		if fmtBuf, err := imports.Process(f.Name(), buf, nil); err == nil {
			_, err = f.Write(fmtBuf)
			return err
		} else {
			log.Printf("[WARN] cannot gofmt %s: %s\n", f.Name(), err.Error())
			f.Write(buf)
			return nil
		}
	}
	_, err := f.Write(buf)
	return err
}

func includePathsFromPkgConfig(opts []string) []string {
	if len(opts) == 0 {
		return nil
	}
	pc, err := pkg.NewConfig(nil)
	if err != nil {
		log.Println("[WARN]", err)
		return nil
	}
	for _, opt := range opts {
		if strings.HasPrefix(opt, "-") || strings.HasPrefix(opt, "--") {
			continue
		}
		if pcPath, err := pc.Locate(opt); err == nil {
			if err := pc.Load(pcPath, true); err != nil {
				log.Println("[WARN] pkg-config:", err)
			}
		} else {
			log.Printf("[WARN] %s.pc referenced in pkg-config options but cannot be found: %s", opt, err.Error())
		}
	}
	flags := pc.CFlags()
	includePaths := make([]string, 0, len(flags))
	for _, flag := range flags {
		if idx := strings.Index(flag, "-I"); idx >= 0 {
			includePaths = append(includePaths, strings.TrimSpace(flag[idx+2:]))
		}
	}
	return includePaths
}
