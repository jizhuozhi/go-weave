package weave_test

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jizhuozhi/go-weave"
)

// UserDAO is pure declaration: no struct, no method bodies, no codegen. This
// is the whole data-access layer, MyBatis style.
type UserDAO interface {
	GetUser(ctx context.Context, id int64) *User
	ListUsers(ctx context.Context, limit int) []string
}

type User struct {
	ID   int64
	Name string
}

// queries plays the role of MyBatis' annotation/XML mapping: method name to
// SQL. Arguments bind to the placeholders in order.
var queries = map[string]string{
	"GetUser":   "SELECT id, name FROM users WHERE id = ?",
	"ListUsers": "SELECT name FROM users LIMIT ?",
}

// rows is a pretend database.
var rows = []User{{1, "ada"}, {2, "grace"}, {3, "ken"}}

// executor turns DAO calls into queries: it inspects the method, binds the
// arguments (skipping the conventional ctx) and produces results — the
// interceptor is the implementation.
func executor(c *weave.Invocation) []reflect.Value {
	sql := queries[c.Method.Name]
	var args []any
	for _, a := range c.Args()[1:] { // [0] is ctx
		args = append(args, a.Interface())
	}
	fmt.Printf("%s  -- bind: %v\n", sql, args)

	switch c.Method.Name {
	case "GetUser":
		for _, u := range rows {
			if u.ID == args[0].(int64) {
				return []reflect.Value{reflect.ValueOf(&u)}
			}
		}
		return []reflect.Value{reflect.Zero(c.Method.Type.Out(0))}
	default: // ListUsers
		n := args[0].(int)
		names := make([]string, 0, n)
		for _, u := range rows {
			if len(names) == n {
				break
			}
			names = append(names, u.Name)
		}
		return []reflect.Value{reflect.ValueOf(names)}
	}
}

func Example_declarativeDAO() {
	// A nil target makes the proxy a pure mock; the interceptor is the DAO
	// implementation. To a caller, dao is just a UserDAO.
	dao := weave.New[UserDAO](nil, executor)

	u := dao.GetUser(context.Background(), 2)
	fmt.Println("found:", u.Name)

	for _, name := range dao.ListUsers(context.Background(), 2) {
		fmt.Println("row:", name)
	}

	// Output:
	// SELECT id, name FROM users WHERE id = ?  -- bind: [2]
	// found: grace
	// SELECT name FROM users LIMIT ?  -- bind: [2]
	// row: ada
	// row: grace
}
