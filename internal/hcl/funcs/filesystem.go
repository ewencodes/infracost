package funcs

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar"
	"github.com/hashicorp/go-cty-funcs/filesystem"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/infracost/infracost/internal/logging"
)

func MakeFileFunc(baseDir string, encBase64 bool) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{
				Name: "path",
				Type: cty.String,
			},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			src, err := readFileBytes(baseDir, path)
			if err != nil {
				err = function.NewArgError(0, err)
				return cty.UnknownVal(cty.String), err
			}

			switch {
			case encBase64:
				enc := base64.StdEncoding.EncodeToString(src)
				return cty.StringVal(enc), nil
			default:
				if !utf8.Valid(src) {
					return cty.UnknownVal(cty.String), fmt.Errorf("contents of %s are not valid UTF-8; use the filebase64 function to obtain the Base64 encoded contents or the other file functions (e.g. filemd5, filesha256) to obtain file hashing results instead", path)
				}
				return cty.StringVal(string(src)), nil
			}
		},
	})
}

// MakeTemplateFileFunc constructs a function that takes a file path and
// an arbitrary object of named values and attempts to render the referenced
// file as a template using HCL template syntax.
//
// The template itself may recursively call other functions so a callback
// must be provided to get access to those functions. The template cannot,
// however, access any variables defined in the scope: it is restricted only to
// those variables provided in the second function argument, to ensure that all
// dependencies on other graph nodes can be seen before executing this function.
//
// As a special exception, a referenced template file may not recursively call
// the templatefile function, since that would risk the same file being
// included into itself indefinitely.
func MakeTemplateFileFunc(baseDir string, funcsCb func() map[string]function.Function) function.Function {

	params := []function.Parameter{
		{
			Name: "path",
			Type: cty.String,
		},
		{
			Name: "vars",
			Type: cty.DynamicPseudoType,
		},
	}

	loadTmpl := func(fn string) (hcl.Expression, error) {
		// We re-use File here to ensure the same filename interpretation
		// as it does, along with its other safety checks.
		tmplVal, err := File(baseDir, cty.StringVal(fn))
		if err != nil {
			return nil, err
		}

		expr, diags := hclsyntax.ParseTemplate([]byte(tmplVal.AsString()), fn, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, diags
		}

		return expr, nil
	}

	renderTmpl := func(expr hcl.Expression, varsVal cty.Value) (cty.Value, error) {
		if varsTy := varsVal.Type(); !(varsTy.IsMapType() || varsTy.IsObjectType()) {
			return cty.DynamicVal, function.NewArgErrorf(1, "invalid vars value: must be a map") // or an object, but we don't strongly distinguish these most of the time
		}

		ctx := &hcl.EvalContext{
			Variables: varsVal.AsValueMap(),
		}

		// We require all of the variables to be valid HCL identifiers, because
		// otherwise there would be no way to refer to them in the template
		// anyway. Rejecting this here gives better feedback to the user
		// than a syntax error somewhere in the template itself.
		for n := range ctx.Variables {
			if !hclsyntax.ValidIdentifier(n) {
				// This error message intentionally doesn't describe _all_ of
				// the different permutations that are technically valid as an
				// HCL identifier, but rather focuses on what we might
				// consider to be an "idiomatic" variable name.
				return cty.DynamicVal, function.NewArgErrorf(1, "invalid template variable name %q: must start with a letter, followed by zero or more letters, digits, and underscores", n)
			}
		}

		// We'll pre-check references in the template here so we can give a
		// more specialized error message than HCL would by default, so it's
		// clearer that this problem is coming from a templatefile call.
		for _, traversal := range expr.Variables() {
			root := traversal.RootName()
			if _, ok := ctx.Variables[root]; !ok {
				return cty.DynamicVal, function.NewArgErrorf(1, "vars map does not contain key %q, referenced at %s", root, traversal[0].SourceRange())
			}
		}

		givenFuncs := funcsCb() // this callback indirection is to avoid chicken/egg problems
		funcs := make(map[string]function.Function, len(givenFuncs))
		for name, fn := range givenFuncs {
			if name == "templatefile" {
				// We stub this one out to prevent recursive calls.
				funcs[name] = function.New(&function.Spec{
					Params: params,
					Type: func(args []cty.Value) (cty.Type, error) {
						return cty.NilType, fmt.Errorf("cannot recursively call templatefile from inside templatefile call")
					},
				})
				continue
			}
			funcs[name] = fn
		}
		ctx.Functions = funcs

		val, diags := expr.Value(ctx)
		if diags.HasErrors() {
			return cty.DynamicVal, diags
		}
		return val, nil
	}

	return function.New(&function.Spec{
		Params: params,
		Type: func(args []cty.Value) (cty.Type, error) {
			if !(args[0].IsKnown() && args[1].IsKnown()) {
				return cty.DynamicPseudoType, nil
			}

			// We'll render our template now to see what result type it produces.
			// A template consisting only of a single interpolation an potentially
			// return any type.
			expr, err := loadTmpl(args[0].AsString())
			if err != nil {
				return cty.DynamicPseudoType, err
			}

			// This is safe even if args[1] contains unknowns because the HCL
			// template renderer itself knows how to short-circuit those.
			val, err := renderTmpl(expr, args[1])
			return val.Type(), err
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			expr, err := loadTmpl(args[0].AsString())
			if err != nil {
				return cty.DynamicVal, err
			}
			return renderTmpl(expr, args[1])
		},
	})

}

// MakeFileExistsFunc constructs a function that takes a path
// and determines whether a file exists at that path
func MakeFileExistsFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{
				Name: "path",
				Type: cty.String,
			},
		},
		Type: function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			path, err := homedir.Expand(path)
			if err != nil {
				return cty.UnknownVal(cty.Bool), fmt.Errorf("failed to expand ~: %s", err)
			}

			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}

			// Ensure that the path is canonical for the host OS
			path = filepath.Clean(path)
			if err := isPathInRepo(path); err != nil {
				logging.Logger.Debug().Msgf("isPathInRepo error: %s returning false for filesytem func", err)

				return cty.False, nil
			}

			fi, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return cty.False, nil
				}
				return cty.UnknownVal(cty.Bool), fmt.Errorf("failed to stat %s", path)
			}

			if fi.Mode().IsRegular() {
				return cty.True, nil
			}

			return cty.False, fmt.Errorf("%s is not a regular file, but %q",
				path, fi.Mode().String())
		},
	})
}

// MakeFileSetFunc constructs a function that takes a glob pattern
// and enumerates a file set from that pattern
func MakeFileSetFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{
				Name: "path",
				Type: cty.String,
			},
			{
				Name: "pattern",
				Type: cty.String,
			},
		},
		Type: function.StaticReturnType(cty.Set(cty.String)),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			pattern := args[1].AsString()

			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}

			// Join the path to the glob pattern, while ensuring the full
			// pattern is canonical for the host OS. The joined path is
			// automatically cleaned during this operation.
			pattern = filepath.Join(path, pattern)

			matches, err := doublestar.Glob(pattern)
			if err != nil {
				return cty.UnknownVal(cty.Set(cty.String)), fmt.Errorf("failed to glob pattern (%s): %s", pattern, err)
			}

			var matchVals []cty.Value
			for _, match := range matches {
				if err := isPathInRepo(match); err != nil {
					logging.Logger.Debug().Msgf("isPathInRepo error: %s skipping match for filesytem func", err)
					continue
				}

				fi, err := os.Stat(match)

				if err != nil {
					return cty.UnknownVal(cty.Set(cty.String)), fmt.Errorf("failed to stat (%s): %s", match, err)
				}

				if !fi.Mode().IsRegular() {
					continue
				}

				// Remove the path and file separator from matches.
				match, err = filepath.Rel(path, match)

				if err != nil {
					return cty.UnknownVal(cty.Set(cty.String)), fmt.Errorf("failed to trim path of match (%s): %s", match, err)
				}

				// Replace any remaining file separators with forward slash (/)
				// separators for cross-system compatibility.
				match = filepath.ToSlash(match)

				matchVals = append(matchVals, cty.StringVal(match))
			}

			if len(matchVals) == 0 {
				return cty.SetValEmpty(cty.String), nil
			}

			return cty.SetVal(matchVals), nil
		},
	})
}

func openFile(baseDir, path string) (io.Reader, error) {
	path, err := homedir.Expand(path)
	if err != nil {
		return nil, fmt.Errorf("failed to expand ~: %s", err)
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}

	// Ensure that the path is canonical for the host OS
	path = filepath.Clean(path)

	if err := isPathInRepo(path); err != nil {
		logging.Logger.Debug().Msgf("isPathInRepo error: %s returning a blank buffer for filesytem func", err)

		return bytes.NewBuffer([]byte{}), nil
	}

	return os.Open(path)
}

func readFileBytes(baseDir, path string) ([]byte, error) {
	f, err := openFile(baseDir, path)
	if err != nil {
		if os.IsNotExist(err) {
			// An extra Terraform-specific hint for this situation
			return nil, fmt.Errorf("no file exists at %s; this function works only with files that are distributed as part of the configuration source code, so if this file will be created by a resource in this configuration you must instead obtain this result from an attribute of that resource", path)
		}
		return nil, err
	}

	src, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s", path)
	}

	return src, nil
}

// File reads the contents of the file at the given path.
//
// The file must contain valid UTF-8 bytes, or this function will return an error.
//
// The underlying function implementation works relative to a particular base
// directory, so this wrapper takes a base directory string and uses it to
// construct the underlying function before calling it.
func File(baseDir string, path cty.Value) (cty.Value, error) {
	fn := MakeFileFunc(baseDir, false)
	return fn.Call([]cty.Value{path})
}

// FileExists determines whether a file exists at the given path.
//
// The underlying function implementation works relative to a particular base
// directory, so this wrapper takes a base directory string and uses it to
// construct the underlying function before calling it.
func FileExists(baseDir string, path cty.Value) (cty.Value, error) {
	fn := MakeFileExistsFunc(baseDir)
	return fn.Call([]cty.Value{path})
}

// FileSet enumerates a set of files given a glob pattern
//
// The underlying function implementation works relative to a particular base
// directory, so this wrapper takes a base directory string and uses it to
// construct the underlying function before calling it.
func FileSet(baseDir string, path, pattern cty.Value) (cty.Value, error) {
	fn := MakeFileSetFunc(baseDir)
	return fn.Call([]cty.Value{path, pattern})
}

// FileBase64 reads the contents of the file at the given path.
//
// The bytes from the file are encoded as base64 before returning.
//
// The underlying function implementation works relative to a particular base
// directory, so this wrapper takes a base directory string and uses it to
// construct the underlying function before calling it.
func FileBase64(baseDir string, path cty.Value) (cty.Value, error) {
	fn := MakeFileFunc(baseDir, true)
	return fn.Call([]cty.Value{path})
}

// Basename takes a string containing a filesystem path and removes all except the last portion from it.
//
// The underlying function implementation works only with the path string and does not access the filesystem itself.
// It is therefore unable to take into account filesystem features such as symlinks.
//
// If the path is empty then the result is ".", representing the current working directory.
func Basename(path cty.Value) (cty.Value, error) {
	return filesystem.BasenameFunc.Call([]cty.Value{path})
}

// Dirname takes a string containing a filesystem path and removes the last portion from it.
//
// The underlying function implementation works only with the path string and does not access the filesystem itself.
// It is therefore unable to take into account filesystem features such as symlinks.
//
// If the path is empty then the result is ".", representing the current working directory.
func Dirname(path cty.Value) (cty.Value, error) {
	return filesystem.DirnameFunc.Call([]cty.Value{path})
}

// Pathexpand takes a string that might begin with a `~` segment, and if so it replaces that segment with
// the current user's home directory path.
//
// The underlying function implementation works only with the path string and does not access the filesystem itself.
// It is therefore unable to take into account filesystem features such as symlinks.
//
// If the leading segment in the path is not `~` then the given path is returned unmodified.
func Pathexpand(path cty.Value) (cty.Value, error) {
	return filesystem.PathExpandFunc.Call([]cty.Value{path})
}

func isPathInRepo(path string) error {
	// isPathInRepo is a no-op when not running in github/gitlab app env.
	ciPlatform := os.Getenv("INFRACOST_CI_PLATFORM")
	if ciPlatform != "github_app" && ciPlatform != "gitlab_app" {
		return nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(wd, path)
	}

	// ensure the path resolves to the real symlink path
	path = symlinkPath(path)

	clean := filepath.Clean(wd)
	if wd != "" && !strings.HasPrefix(path, clean) {
		return fmt.Errorf("file %s is not within the repository directory %s", path, wd)
	}

	return nil
}

// symlinkPath checks the given file path and returns the real path if it is a
// symlink.
func symlinkPath(filepathStr string) string {
	fileInfo, err := os.Lstat(filepathStr)
	if err != nil {
		return filepathStr
	}

	if fileInfo.Mode()&os.ModeSymlink != 0 {
		realPath, err := filepath.EvalSymlinks(filepathStr)
		if err != nil {
			return filepathStr
		}

		return realPath
	}

	return filepathStr
}
