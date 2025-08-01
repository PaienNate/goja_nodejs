package require

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	js "github.com/dop251/goja"
)

const NodePrefix = "node:"

func (r *RequireModule) resolvePath(base, name string) string {
	if r.r.pathResolver != nil {
		return r.r.pathResolver(base, name)
	}
	return DefaultPathResolver(base, name)
}

// NodeJS module search algorithm described by
// https://nodejs.org/api/modules.html#modules_all_together
func (r *RequireModule) resolve(modpath string) (module *js.Object, err error) {
	var start string
	err = nil
	if !filepath.IsAbs(modpath) {
		start = r.getCurrentModulePath()
	}

	p := r.resolvePath(start, modpath)
	if isFileOrDirectoryPath(modpath) {
		if module, ok := r.modules.Load(p); ok {
			return module, nil
		}
		module, err = r.loadAsFileOrDirectory(p)
		if err == nil && module != nil {
			r.modules.Store(p, module)
		}
	} else {
		module, err = r.loadNative(modpath)
		if err == nil {
			return
		} else {
			if err == InvalidModuleError {
				err = nil
			} else {
				return
			}
		}
		if module, ok := r.nodeModules.Load(p); ok {
			return module, nil
		}
		module, err = r.loadNodeModules(modpath, start)
		if err == nil && module != nil {
			r.nodeModules.Store(p, module)
		}
	}

	if module == nil && err == nil {
		err = InvalidModuleError
	}
	return
}

func (r *RequireModule) loadNative(path string) (*js.Object, error) {
	if module, ok := r.modules.Load(path); ok {
		return module, nil
	}

	var ldr ModuleLoader
	ldr, _ = r.r.native.Load(path)
	if ldr == nil {
		ldr, _ = native.Load(path)
	}

	var isBuiltIn, withPrefix bool
	if ldr == nil {
		ldr, _ = builtin.Load(path)
		if ldr == nil && strings.HasPrefix(path, NodePrefix) {
			ldr, _ = builtin.Load(path[len(NodePrefix):])
			if ldr == nil {
				return nil, NoSuchBuiltInModuleError
			}
			withPrefix = true
		}
		isBuiltIn = true
	}

	if ldr != nil {
		module := r.createModuleObject()
		r.modules.Store(path, module)
		if isBuiltIn {
			if withPrefix {
				r.modules.Store(path[len(NodePrefix):], module)
			} else {
				if !strings.HasPrefix(path, NodePrefix) {
					r.modules.Store(NodePrefix+path, module)
				}
			}
		}
		ldr(r.runtime, module)
		return module, nil
	}

	return nil, InvalidModuleError
}

func (r *RequireModule) loadAsFileOrDirectory(path string) (module *js.Object, err error) {
	if module, err = r.loadAsFile(path); module != nil || err != nil {
		return
	}

	return r.loadAsDirectory(path)
}

func (r *RequireModule) loadAsFile(path string) (module *js.Object, err error) {
	if module, err = r.loadModule(path); module != nil || err != nil {
		return
	}

	p := path + ".js"
	if module, err = r.loadModule(p); module != nil || err != nil {
		return
	}

	p = path + ".json"
	return r.loadModule(p)
}

func (r *RequireModule) loadIndex(modpath string) (module *js.Object, err error) {
	p := r.resolvePath(modpath, "index.js")
	if module, err = r.loadModule(p); module != nil || err != nil {
		return
	}

	p = r.resolvePath(modpath, "index.json")
	return r.loadModule(p)
}

func (r *RequireModule) loadAsDirectory(modpath string) (module *js.Object, err error) {
	p := r.resolvePath(modpath, "package.json")
	buf, err := r.r.getSource(p)
	if err != nil {
		return r.loadIndex(modpath)
	}
	var pkg struct {
		Main string
	}
	err = json.Unmarshal(buf, &pkg)
	if err != nil || len(pkg.Main) == 0 {
		return r.loadIndex(modpath)
	}

	m := r.resolvePath(modpath, pkg.Main)
	if module, err = r.loadAsFile(m); module != nil || err != nil {
		return
	}

	return r.loadIndex(m)
}

func (r *RequireModule) loadNodeModule(modpath, start string) (*js.Object, error) {
	return r.loadAsFileOrDirectory(r.resolvePath(start, modpath))
}

func (r *RequireModule) loadNodeModules(modpath, start string) (module *js.Object, err error) {
	for _, dir := range r.r.globalFolders {
		if module, err = r.loadNodeModule(modpath, dir); module != nil || err != nil {
			return
		}
	}
	for {
		var p string
		if filepath.Base(start) != "node_modules" {
			p = filepath.Join(start, "node_modules")
		} else {
			p = start
		}
		if module, err = r.loadNodeModule(modpath, p); module != nil || err != nil {
			return
		}
		if start == ".." { // Dir('..') is '.'
			break
		}
		parent := filepath.Dir(start)
		if parent == start {
			break
		}
		start = parent
	}

	return
}

func (r *RequireModule) getCurrentModulePath() string {
	var buf [2]js.StackFrame
	frames := r.runtime.CaptureCallStack(2, buf[:0])
	if len(frames) < 2 {
		return "."
	}
	return filepath.Dir(frames[1].SrcName())
}

func (r *RequireModule) createModuleObject() *js.Object {
	module := r.runtime.NewObject()
	module.Set("exports", r.runtime.NewObject())
	return module
}

func (r *RequireModule) loadModule(path string) (*js.Object, error) {
	if module, ok := r.modules.Load(path); ok {
		return module, nil
	}
	
	module := r.createModuleObject()
	r.modules.Store(path, module)
	err := r.loadModuleFile(path, module)
	if err != nil {
		module = nil
		r.modules.Delete(path)
		if errors.Is(err, ModuleFileDoesNotExistError) {
			err = nil
		}
	}
	return module, err
}

func (r *RequireModule) loadModuleFile(path string, jsModule *js.Object) error {

	prg, err := r.r.getCompiledSource(path)

	if err != nil {
		return err
	}

	f, err := r.runtime.RunProgram(prg)
	if err != nil {
		return err
	}

	if call, ok := js.AssertFunction(f); ok {
		jsExports := jsModule.Get("exports")
		jsRequire := r.runtime.Get("require")

		// Run the module source, with "jsExports" as "this",
		// "jsExports" as the "exports" variable, "jsRequire"
		// as the "require" variable and "jsModule" as the
		// "module" variable (Nodejs capable).
		_, err = call(jsExports, jsExports, jsRequire, jsModule, r.runtime.ToValue(path), r.runtime.ToValue(filepath.Dir(path)))
		if err != nil {
			return err
		}
	} else {
		return InvalidModuleError
	}

	return nil
}

func isFileOrDirectoryPath(path string) bool {
	result := path == "." || path == ".." ||
		strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, "./") ||
		strings.HasPrefix(path, "../")

	if runtime.GOOS == "windows" {
		result = result ||
			strings.HasPrefix(path, `.\`) ||
			strings.HasPrefix(path, `..\`) ||
			filepath.IsAbs(path)
	}

	return result
}
