package interp

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// importSrc calls global tag analysis on the source code for the package identified by
// importPath. rPath is the relative path to the directory containing the source
// code for the package. It can also be "main" as a special value.
func (interp *Interpreter) importSrc(rPath, importPath string, skipTest bool) (string, error) {
	var dir string
	var err error

	if interp.srcPkg[importPath] != nil {
		name, ok := interp.pkgNames[importPath]
		if !ok {
			return "", fmt.Errorf("inconsistent knowledge about %s", importPath)
		}
		return name, nil
	}

	// resolve relative and absolute import paths
	if isPathRelative(importPath) {
		if rPath == mainID {
			rPath = "."
		}
		dir = filepath.Join(filepath.Dir(interp.name), rPath, importPath)
	} else {
		if dir, err = interp.getPackageDir(importPath); err != nil {
			return "", err
		}
	}

	if interp.rdir[importPath] {
		return "", fmt.Errorf("import cycle not allowed\n\timports %s", importPath)
	}
	interp.rdir[importPath] = true

	files, err := fs.ReadDir(interp.opt.filesystem, dir)
	if err != nil {
		return "", err
	}

	var initNodes []*node
	var rootNodes []*node
	revisit := make(map[string][]*node)

	var root *node
	var pkgName string

	// Parse source files.
	for _, file := range files {
		name := file.Name()
		if skipFile(&interp.context, name, skipTest) {
			continue
		}

		name = filepath.Join(dir, name)
		var buf []byte
		if buf, err = fs.ReadFile(interp.opt.filesystem, name); err != nil {
			return "", err
		}

		n, err := interp.parse(string(buf), name, false)
		if err != nil {
			return "", err
		}
		if n == nil {
			continue
		}

		var pname string
		if pname, root, err = interp.ast(n); err != nil {
			return "", err
		}
		if root == nil {
			continue
		}

		if interp.astDot {
			dotCmd := interp.dotCmd
			if dotCmd == "" {
				dotCmd = defaultDotCmd(name, "yaegi-ast-")
			}
			root.astDot(dotWriter(dotCmd), name)
		}
		if pkgName == "" {
			pkgName = pname
		} else if pkgName != pname && skipTest {
			return "", fmt.Errorf("found packages %s and %s in %s", pkgName, pname, dir)
		}
		rootNodes = append(rootNodes, root)

		subRPath := effectivePkg(rPath, importPath)
		var list []*node
		list, err = interp.gta(root, subRPath, importPath, pkgName)
		if err != nil {
			return "", err
		}
		revisit[subRPath] = append(revisit[subRPath], list...)
	}

	// Revisit incomplete nodes where GTA could not complete.
	for _, nodes := range revisit {
		if err = interp.gtaRetry(nodes, importPath, pkgName); err != nil {
			return "", err
		}
	}

	// Generate control flow graphs.
	for _, root := range rootNodes {
		var nodes []*node
		if nodes, err = interp.cfg(root, nil, importPath, pkgName); err != nil {
			return "", err
		}
		initNodes = append(initNodes, nodes...)
	}

	// Register source package in the interpreter. The package contains only
	// the global symbols in the package scope.
	interp.mutex.Lock()
	gs := interp.scopes[importPath]
	interp.srcPkg[importPath] = gs.sym
	interp.pkgNames[importPath] = pkgName

	interp.frame.mutex.Lock()
	interp.resizeFrame()
	interp.frame.mutex.Unlock()
	interp.mutex.Unlock()

	// Once all package sources have been parsed, execute entry points then init functions.
	for _, n := range rootNodes {
		if err = genRun(n); err != nil {
			return "", err
		}
		interp.run(n, nil)
	}

	// Wire and execute global vars in global scope gs.
	n, err := genGlobalVars(rootNodes, gs)
	if err != nil {
		return "", err
	}
	interp.run(n, nil)

	// Add main to list of functions to run, after all inits.
	if m := gs.sym[mainID]; pkgName == mainID && m != nil && skipTest {
		initNodes = append(initNodes, m.node)
	}

	for _, n := range initNodes {
		interp.run(n, interp.frame)
	}

	return pkgName, nil
}

// rootFromSourceLocation returns the path to the directory containing the input
// Go file given to the interpreter, relative to $GOPATH/src.
// It is meant to be called in the case when the initial input is a main package.
func (interp *Interpreter) rootFromSourceLocation() (string, error) {
	sourceFile := interp.name
	if sourceFile == DefaultSourceName {
		return "", nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	pkgDir := filepath.Join(wd, filepath.Dir(sourceFile))
	root := strings.TrimPrefix(pkgDir, filepath.Join(interp.context.GOPATH, "src")+"/")
	if root == wd {
		return "", fmt.Errorf("package location %s not in GOPATH", pkgDir)
	}
	return root, nil
}

// getPackageDir uses the GOPATH to find the absolute path of an import path
func (interp *Interpreter) getPackageDir(importPath string) (string, error) {
	// search the standard library and Go modules.
	config := packages.Config{}
	config.Env = append(config.Env, "GOPATH="+interp.context.GOPATH, "GOCACHE="+interp.opt.env["goCache"], "GOTOOLDIR="+interp.opt.env["goToolDir"])
	pkgs, err := packages.Load(&config, importPath)
	if err != nil {
		return "", fmt.Errorf("An error occurred retrieving a package from the GOPATH: %v\n%v\nIf Access is denied, run in administrator.", importPath, err)
	}

	// confirm the import path is found.
	for _, pkg := range pkgs {
		for _, goFile := range pkg.GoFiles {
			if strings.Contains(filepath.Dir(goFile), pkg.Name) {
				return filepath.Dir(goFile), nil
			}
		}
	}

	// check for certain go tools located in GOTOOLDIR
	if interp.opt.env["goToolDir"] != "" {
		// search for the go directory before searching for packages
		// this approach prevents the computer from searching the entire filesystem
		godir, err := searchUpDirPath(interp.opt.env["goToolDir"], "go", false)
		if err != nil {
			return "", fmt.Errorf("An import source could not be found: %q\nThe current GOPATH=%v, GOCACHE=%v, GOTOOLDIR=%v\n%v", importPath, interp.context.GOPATH, interp.opt.env["goCache"], interp.opt.env["goToolDir"], err)
		}

		absimportpath, err := searchDirs(godir, importPath)
		if err != nil {
			return "", fmt.Errorf("An import source could not be found: %q\nThe current GOPATH=%v, GOCACHE=%v, GOTOOLDIR=%v\n%v", importPath, interp.context.GOPATH, interp.opt.env["goCache"], interp.opt.env["goToolDir"], err)
		}
		return absimportpath, nil
	}
	return "", fmt.Errorf("An import source could not be found: %q. Set the GOPATH and/or GOTOOLDIR environment variable from Interpreter.Options.", importPath)
}

// searchUpDirPath searches up a directory path in order to find a target directory.
func searchUpDirPath(initial string, target string, isCaseSensitive bool) (string, error) {
	// strings.Split always returns [:0] as filepath.Dir returns "." or the last directory
	splitdir := strings.Split(filepath.Join(initial), string(filepath.Separator))
	if len(splitdir) == 1 {
		return "", fmt.Errorf("The target directory %q is not within the path %q", target, initial)
	}

	updir := splitdir[len(splitdir)-1]
	if !isCaseSensitive {
		updir = strings.ToLower(updir)
	}
	if updir == target {
		return initial, nil
	}
	return searchUpDirPath(filepath.Dir(initial), target, isCaseSensitive)
}

// searchDirs searches within a directory (and its subdirectories) in an attempt to find a filepath.
func searchDirs(initial string, target string) (string, error) {
	absfilepath, err := filepath.Abs(initial)
	if err != nil {
		return "", err
	}

	// find the go directory
	var foundpath string
	filter := func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			if d.Name() == target {
				foundpath = path
			}
		}
		return nil
	}
	if err = filepath.WalkDir(absfilepath, filter); err != nil {
		return "", fmt.Errorf("An error occurred searching for a directory.\n%v", err)
	}

	if foundpath != "" {
		return foundpath, nil
	}
	return "", fmt.Errorf("The target filepath %q is not within the path %q", target, initial)
}

func effectivePkg(root, path string) string {
	splitRoot := strings.Split(root, string(filepath.Separator))
	splitPath := strings.Split(path, string(filepath.Separator))

	var result []string

	rootIndex := 0
	prevRootIndex := 0
	for i := 0; i < len(splitPath); i++ {
		part := splitPath[len(splitPath)-1-i]

		index := len(splitRoot) - 1 - rootIndex
		if index > 0 && part == splitRoot[index] && i != 0 {
			prevRootIndex = rootIndex
			rootIndex++
		} else if prevRootIndex == rootIndex {
			result = append(result, part)
		}
	}

	var frag string
	for i := len(result) - 1; i >= 0; i-- {
		frag = filepath.Join(frag, result[i])
	}

	return filepath.Join(root, frag)
}

// isPathRelative returns true if path starts with "./" or "../".
// It is intended for use on import paths, where "/" is always the directory separator.
func isPathRelative(s string) bool {
	return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}
