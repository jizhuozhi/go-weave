package main

// 声明式 DAO：接口即数据访问层，无实现结构体、无 codegen。
//
// 分两阶段：先 compileMapper 把 userdao.xml 预编译成执行计划，再用
// weave.New 安装 interceptor。执行时只查内存里的计划，不碰文件——改 SQL 后
// 显式调用 reload() 才热生效。方法名、WHERE/LIMIT、返回类型全部来自 XML 和
// 接口签名，没有一处硬编码。运行：
//
//	cd examples/dao && go run .

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/jizhuozhi/go-weave"
)

// UserDAO 是纯粹声明：没有实现结构体、没有 codegen。
type UserDAO interface {
	GetUser(ctx context.Context, id int64) (*User, error)
	ListUsers(ctx context.Context, limit int) ([]User, error)
}

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// rows 是"数据库"里的行，序列化成 JSON 存着。
var rows = [][]byte{
	[]byte(`{"id":1,"name":"ada"}`),
	[]byte(`{"id":2,"name":"grace"}`),
	[]byte(`{"id":3,"name":"ken"}`),
}

// ---- 阶段一：预编译 XML → 执行计划 ----

type mapperXML struct {
	Selects []struct {
		ID  string `xml:"id,attr"`
		SQL string `xml:",chardata"`
	} `xml:"select"`
}

// stmt 是一条语句的预编译计划。绑定信息全部在预编译时从 SQL 里提取。
type stmt struct {
	sql      string
	params   []string // #{...} 按出现顺序
	whereCol string   // WHERE 的列名（空 = 无 WHERE）
	whereIdx int      // WHERE 参数在 params 里的索引（-1 = 无）
	limitIdx int      // LIMIT 参数在 params 里的索引（-1 = 无）
}

var (
	paramRe = regexp.MustCompile(`#\{(\w+)\}`)
	whereRe = regexp.MustCompile(`(?i)WHERE\s+(\w+)\s*=\s*#\{(\w+)\}`)
	limitRe = regexp.MustCompile(`(?i)LIMIT\s*#\{(\w+)\}`)
)

func compileStmt(sql string) *stmt {
	sql = strings.TrimSpace(sql)
	st := &stmt{sql: sql, whereIdx: -1, limitIdx: -1}
	for _, m := range paramRe.FindAllStringSubmatch(sql, -1) {
		st.params = append(st.params, m[1])
	}
	if m := whereRe.FindStringSubmatch(sql); m != nil {
		st.whereCol = m[1]
		st.whereIdx = indexOf(st.params, m[2])
	}
	if m := limitRe.FindStringSubmatch(sql); m != nil {
		st.limitIdx = indexOf(st.params, m[1])
	}
	return st
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// compiled 是预编译好的映射。执行时只查内存计划，不再碰文件。
type compiled struct {
	path  string
	stmts map[string]*stmt
}

func compileMapper(path string) (*compiled, error) {
	c := &compiled{path: path, stmts: map[string]*stmt{}}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// reload 重新读文件并预编译。部署时替换文件后显式调用即可热生效。
func (c *compiled) reload() error {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var m mapperXML
	if err := xml.Unmarshal(raw, &m); err != nil {
		return err
	}
	for _, s := range m.Selects {
		c.stmts[s.ID] = compileStmt(s.SQL)
	}
	return nil
}

// run 通用执行：按 SQL 里的 WHERE 等值过滤 + LIMIT，逐行匹配。
func (st *stmt) run(bound []any) [][]byte {
	var hit [][]byte
	for _, r := range rows {
		if st.whereIdx >= 0 {
			var m map[string]any
			if err := json.Unmarshal(r, &m); err != nil {
				continue
			}
			if fmt.Sprint(m[st.whereCol]) != fmt.Sprint(bound[st.whereIdx]) {
				continue
			}
		}
		hit = append(hit, r)
		if st.limitIdx >= 0 && len(hit) >= int(bound[st.limitIdx].(int)) {
			break
		}
	}
	return hit
}

// unmarshal 按返回值类型反序列化：指针→单行，切片→多行。
func unmarshal(ret reflect.Type, rows [][]byte) (reflect.Value, error) {
	switch ret.Kind() {
	case reflect.Ptr:
		p := reflect.New(ret.Elem())
		if len(rows) > 0 {
			if err := json.Unmarshal(rows[0], p.Interface()); err != nil {
				return reflect.Value{}, err
			}
		}
		return p, nil
	case reflect.Slice:
		s := reflect.MakeSlice(ret, 0, len(rows))
		for _, r := range rows {
			e := reflect.New(ret.Elem())
			if err := json.Unmarshal(r, e.Interface()); err != nil {
				return reflect.Value{}, err
			}
			s = reflect.Append(s, e.Elem())
		}
		return s, nil
	}
	return reflect.Value{}, fmt.Errorf("unsupported return type %s", ret)
}

// ---- 阶段二：用预编译计划安装 interceptor ----

func (c *compiled) interceptor() weave.Interceptor {
	return func(inv *weave.Invocation) []reflect.Value {
		st := c.stmts[inv.Method.Name]
		if st == nil {
			return zeroResults(inv)
		}
		args := inv.Args()[1:] // [0] 是 ctx
		bound := make([]any, len(args))
		for i, a := range args {
			bound[i] = a.Interface()
		}
		fmt.Printf("%s  -- bind: %v\n", paramRe.ReplaceAllString(st.sql, "?"), bound)

		hit := st.run(bound)

		ret := inv.Method.Type.Out(0)
		if len(hit) == 0 && ret.Kind() == reflect.Ptr {
			return []reflect.Value{
				reflect.Zero(ret),
				reflect.ValueOf(fmt.Errorf("%s: not found", inv.Method.Name)),
			}
		}
		v, err := unmarshal(ret, hit)
		if err != nil {
			return []reflect.Value{reflect.Zero(ret), reflect.ValueOf(err)}
		}
		return []reflect.Value{v, reflect.Zero(inv.Method.Type.Out(1))}
	}
}

func zeroResults(inv *weave.Invocation) []reflect.Value {
	out := make([]reflect.Value, inv.Method.NumOut())
	for i := range out {
		out[i] = reflect.Zero(inv.Method.Type.Out(i))
	}
	return out
}

func main() {
	file := flag.String("file", "userdao.xml", "path to the mapper XML")
	flag.Parse()

	// 阶段一：预编译
	compiled, err := compileMapper(*file)
	if err != nil {
		panic(err)
	}

	// 阶段二：安装 interceptor
	dao := weave.New[UserDAO](nil, compiled.interceptor())

	ctx := context.Background()

	u, err := dao.GetUser(ctx, 2)
	fmt.Println("found:", u.Name, err)

	users, _ := dao.ListUsers(ctx, 2)
	fmt.Println("rows:", users)

	missing, err := dao.GetUser(ctx, 99)
	fmt.Println("missing:", missing, err)
}
