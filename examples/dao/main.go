package main

// A declarative DAO: the interface *is* the data access layer — no
// implementation struct, no codegen.
//
// Two phases. First compileMapper pre-compiles userdao.xml into execution
// plans, then weave.New installs the interceptor. Execution reads only the
// in-memory plans and never touches the file — call reload() explicitly to pick
// up edited SQL. Method names, WHERE/LIMIT binding and return types all come
// from the XML and the interface signature; nothing is hardcoded.
//
// Run:
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

// UserDAO is pure declaration: no implementation struct, no codegen.
type UserDAO interface {
	GetUser(ctx context.Context, id int64) (*User, error)
	ListUsers(ctx context.Context, limit int) ([]User, error)
}

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// rows is the "database": rows serialised as JSON.
var rows = [][]byte{
	[]byte(`{"id":1,"name":"ada"}`),
	[]byte(`{"id":2,"name":"grace"}`),
	[]byte(`{"id":3,"name":"ken"}`),
}

// Phase one: compile the XML into execution plans.

type mapperXML struct {
	Selects []struct {
		ID  string `xml:"id,attr"`
		SQL string `xml:",chardata"`
	} `xml:"select"`
}

// stmt is one statement's pre-compiled plan. Every binding detail is extracted
// from the SQL at compile time.
type stmt struct {
	sql      string
	params   []string // #{...} in order of appearance
	whereCol string   // the WHERE column (empty = no WHERE)
	whereIdx int      // index of the WHERE parameter in params (-1 = none)
	limitIdx int      // index of the LIMIT parameter in params (-1 = none)
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

// compiled is the pre-compiled mapping. Execution reads only the in-memory
// plans and never touches the file again.
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

// reload re-reads the file and re-compiles it. Replace the file in a deployment
// and call this explicitly to pick the change up.
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

// run is the generic executor: equality filter on the SQL's WHERE column, then
// LIMIT, matching row by row.
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

// unmarshal decodes by return type: a pointer yields a single row, a slice
// many.
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

// Phase two: install the interceptor over the pre-compiled plans.

func (c *compiled) interceptor() weave.Interceptor {
	return func(inv *weave.Invocation) []reflect.Value {
		st := c.stmts[inv.Method.Name]
		if st == nil {
			return zeroResults(inv)
		}
		args := inv.Args()[1:] // [0] is ctx
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

	// Phase one: pre-compile.
	compiled, err := compileMapper(*file)
	if err != nil {
		panic(err)
	}

	// Phase two: install the interceptor.
	dao := weave.New[UserDAO](nil, compiled.interceptor())

	ctx := context.Background()

	u, err := dao.GetUser(ctx, 2)
	fmt.Println("found:", u.Name, err)

	users, _ := dao.ListUsers(ctx, 2)
	fmt.Println("rows:", users)

	missing, err := dao.GetUser(ctx, 99)
	fmt.Println("missing:", missing, err)
}
