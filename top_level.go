package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func listTopLevelLicenses(gopath string, pkgs []string) ([]License, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	deps, err := listAllImports(os.Args[1])
	if err != nil {
		if _, ok := err.(*MissingError); ok {
			return nil, err
		}
		return nil, fmt.Errorf("could not list %s dependencies: %s",
			strings.Join(pkgs, " "), err)
	}
	std, err := listStandardPackages(gopath)
	if err != nil {
		return nil, fmt.Errorf("could not list standard packages: %s", err)
	}
	stdSet := map[string]bool{}
	for _, n := range std {
		stdSet[n] = true
	}
	infos, err := getTopLevelPackagesInfo(gopath, deps)
	if err != nil {
		return nil, err
	}

	// Cache matched licenses by path. Useful for package with a lot of
	// subpackages like bleve.
	matched := map[string]MatchResult{}

	licenses := []License{}
	for _, info := range infos {
		if info.Error != nil {
			licenses = append(licenses, License{
				Package: info.Name,
				Err:     info.Error.Err,
			})
			continue
		}
		if stdSet[info.ImportPath] {
			continue
		}
		path, err := findLicense(info)
		if err != nil {
			return nil, err
		}
		license := License{
			Package: info.ImportPath,
			Path:    path,
		}
		if path != "" {
			fpath := filepath.Join(info.Root, "src", path)
			m, ok := matched[fpath]
			if !ok {
				data, err := ioutil.ReadFile(fpath)
				if err != nil {
					return nil, err
				}
				m = matchTemplates(data, templates)
				matched[fpath] = m
			}
			license.Score = m.Score
			license.Template = m.Template
			license.ExtraWords = m.ExtraWords
			license.MissingWords = m.MissingWords
		}
		licenses = append(licenses, license)
	}
	return licenses, nil
}

func getTopLevelPackagesInfo(gopath string, pkgs []string) ([]*PkgInfo, error) {
	args := []string{"list", "-e", "-json"}
	// TODO: split the list for platforms which do not support massive argument
	// lists.
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Env = fixEnv(gopath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go %s failed with:\n%s",
			strings.Join(args, " "), string(out))
	}
	infos := make([]*PkgInfo, 0, len(pkgs))
	decoder := json.NewDecoder(bytes.NewBuffer(out))
	for _, pkg := range pkgs {
		info := &PkgInfo{}
		err := decoder.Decode(info)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve package information for %s", pkg)
		}
		if pkg != info.ImportPath {
			return nil, fmt.Errorf("package information mismatch: asked for %s, got %s",
				pkg, info.ImportPath)
		}
		if info.Error != nil && info.Name == "" {
			cmd = exec.Command("go", "list", "-e", "-json", filepath.Join("src.sevone.com/eng/platform/apical/vendor", info.ImportPath))
			cmd.Env = fixEnv(gopath)
			tryVendor, err := cmd.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("go %s failed with:\n%s",
					strings.Join([]string{"list", "-e", "-json", filepath.Join("src.sevone.com/eng/platform/apical/vendor", info.ImportPath)}, " "), string(tryVendor))
			}
			tryDecoder := json.NewDecoder(bytes.NewBuffer(tryVendor))
			tryInfo := &PkgInfo{}
			err = tryDecoder.Decode(tryInfo)
			if err != nil {
				return nil, fmt.Errorf("could not retrieve package information for %s", pkg)
			}

			if !(tryInfo.Error != nil && tryInfo.Name == "") {
				info = tryInfo
			} else {
				info.Name = info.ImportPath
			}
		}
		infos = append(infos, info)
	}
	return infos, err
}

func listAllImports(fPath string) ([]string, error) {
	// list all go file
	dir := fPath
	subDirToSkip := filepath.Join(fPath, "vendor")

	var goFilePaths []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			//fmt.Printf("prevent panic by handling failure accessing a path %q: %v\n", dir, err)
			return err
		}
		if info.IsDir() && info.Name() == subDirToSkip {
			//fmt.Printf("skipping a dir without errors: %+v \n", info.Name())
			return filepath.SkipDir
		}
		if strings.HasSuffix(info.Name(), ".go") {
			//fmt.Printf("visited file: %q\n", info.Name())
			goFilePaths = append(goFilePaths, path)
		}
		return nil
	})

	if err != nil {
		fmt.Printf("error walking the path %q: %v\n", dir, err)
	}
	//goFilePaths = strings.Split(string(out), "\n")

	imports := make(map[string]struct{})
	// open file
	for _, fPath := range goFilePaths {
		if fPath == "" {
			continue
		}
		goFile, err := os.Open(fPath)
		if err != nil {
			return nil, fmt.Errorf("open file %s error: %s", fPath, err)
		}
		defer goFile.Close()

		reader := bufio.NewReader(goFile)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				} else {
					return nil, fmt.Errorf("read line error in file %s", fPath)
				}
			}

			if strings.Contains(line, "import") {
				// start
				if strings.Contains(line, "(") {
					strImports, err := parseImportBlock(reader)
					if err != nil {
						return nil, fmt.Errorf("%s in file %s", err, fPath)
					}
					for _, str := range strImports {
						if _, ok := imports[str]; !ok {
							imports[str] = struct{}{}
						}
					}
				} else {
					re := regexp.MustCompile(`"[^"]+"`)
					findResult := re.FindString(line)
					if findResult != "" {
						pkg := strings.Trim(findResult, "\"")
						if _, ok := imports[pkg]; !ok {
							imports[pkg] = struct{}{}
						}
					}
				}
			}

			if strings.Contains(line, "func") {
				break
			}
		}
	}

	var strImports []string
	for key := range imports {
		strImports = append(strImports, key)
	}

	return strImports, nil
}

func parseImportBlock(reader *bufio.Reader) ([]string, error) {
	var imports []string
	re := regexp.MustCompile(`"[^"]+"`)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, errors.New("read line error")
			}
		}

		findResult := re.FindString(line)
		if findResult != "" {
			imports = append(imports, strings.Trim(findResult, "\""))
		}

		if strings.Contains(line, ")") {
			break
		}
	}

	return imports, nil
}
