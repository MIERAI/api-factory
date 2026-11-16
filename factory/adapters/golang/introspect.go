package golang

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// sqlc 生成的 Go 代码(gen/db)的结构: 用 go/ast 读,不靠猜 sqlc 的命名规则
// ---------------------------------------------------------------------------

type goField struct {
	Name string // CreatedAt
	Type string // time.Time / *int32 / ...
}

type queryFunc struct {
	ParamName  string // 单参数时的参数名(jobID);结构体参数时为 arg
	ParamType  string // int64 / ListJobsParams;无参数时为空
	ReturnType string // int32 / []Job / GetXRow
}

type dbPackage struct {
	structs map[string][]goField
	funcs   map[string]queryFunc
}

func loadDBPackage(dir string) (*dbPackage, error) {
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	pkg := &dbPackage{structs: map[string][]goField{}, funcs: map[string]queryFunc{}}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					fields := []goField{} // 空结构体(没有参数的查询)也要算作结构体
					for _, fl := range st.Fields.List {
						for _, n := range fl.Names {
							fields = append(fields, goField{Name: n.Name, Type: exprString(fl.Type)})
						}
					}
					pkg.structs[ts.Name.Name] = fields
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) != 1 || exprString(d.Recv.List[0].Type) != "*Queries" {
					continue
				}
				params := d.Type.Params.List
				res := d.Type.Results
				if res == nil || len(res.List) != 2 {
					continue
				}
				q := queryFunc{ReturnType: exprString(res.List[0].Type)}
				if len(params) == 2 { // (ctx, arg)
					q.ParamName = params[1].Names[0].Name
					q.ParamType = exprString(params[1].Type)
				}
				pkg.funcs[d.Name.Name] = q
			}
		}
	}
	return pkg, nil
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	}
	return fmt.Sprintf("%T", e)
}

// rowFields 返回查询结果一行的字段;标量结果返回 nil。
func (p *dbPackage) rowFields(q queryFunc) ([]goField, string) {
	t := strings.TrimPrefix(q.ReturnType, "[]")
	if f, ok := p.structs[t]; ok {
		return f, t
	}
	return nil, t
}

// goCamelCase 与 protoc-gen-go 的字段命名规则一致(job_type → JobType)。
func goCamelCase(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' && i+1 < len(s) && isLower(s[i+1]):
		case c == '.':
			b = append(b, '_')
		case c == '_' && (i == 0 || s[i-1] == '.'):
			b = append(b, 'X')
		case c == '_' && i+1 < len(s) && isLower(s[i+1]):
		case c >= '0' && c <= '9':
			b = append(b, c)
		default:
			if isLower(c) {
				c -= 'a' - 'A'
			}
			b = append(b, c)
			for ; i+1 < len(s) && isLower(s[i+1]); i++ {
				b = append(b, s[i+1])
			}
		}
	}
	return string(b)
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }

// norm 用于名字对应: 忽略大小写和下划线(job_type ≈ JobType ≈ jobType)。
func norm(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}
