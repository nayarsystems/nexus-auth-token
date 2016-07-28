package main

import (
	"log"
	"os"
	"time"

	r "github.com/dancannon/gorethink"
	"github.com/jaracil/ei"
	"github.com/jaracil/nxcli/demos/go/sugar"
	"github.com/jaracil/nxcli/nxcore"
	"github.com/jessevdk/go-flags"
)

var opts struct {
	Host   string `short:"s" long:"server" default:"ws://localhost:8088" description:"Nexus server"`
	Prefix string `short:"p" description:"Nexus prefix to pull" default:"sys.login.token"`
	User   string `long:"user" description:"Nexus username" required:"true"`
	Pass   string `long:"pass" description:"Nexus password" required:"true"`

	Rethink RethinkOptions `group:"RethinkDB Options"`
}

type RethinkOptions struct {
	Host     string `short:"r" long:"rethinkdb" description:"RethinkDB host[:port]" default:"localhost:28015"`
	Database string `long:"db" description:"RethinkDB database" default:"nexusTokenAuth"`
	User     string `long:"ruser" description:"RethinkDB username" default:""`
	Pass     string `long:"rpass" description:"RethinkDB password" default:""`
}

var db *r.Session

func dbOpen() (err error) {
	db, err = r.Connect(r.ConnectOpts{
		Address:  opts.Rethink.Host,
		Database: opts.Rethink.Database,
		MaxIdle:  50,
		MaxOpen:  200,
		Username: opts.Rethink.User,
		Password: opts.Rethink.Pass,
	})
	return
}

func dbBootstrap() error {
	cur, err := r.DBList().Run(db)
	if err != nil {
		return err
	}
	dblist := make([]string, 0)
	err = cur.All(&dblist)
	cur.Close()
	if err != nil {
		return err
	}
	dbexists := false
	for _, x := range dblist {
		if x == opts.Rethink.Database {
			dbexists = true
			break
		}
	}
	if !dbexists {
		_, err := r.DBCreate(opts.Rethink.Database).RunWrite(db)
		if err != nil {
			return err
		}
	}

	cur, err = r.TableList().Run(db)
	if err != nil {
		return err
	}
	tablelist := make([]string, 0)
	err = cur.All(&tablelist)
	cur.Close()
	if err != nil {
		return err
	}
	if !inStrSlice(tablelist, "tokens") {
		log.Println("Creating tokens table")
		_, err := r.TableCreate("tokens").RunWrite(db)
		if err != nil {
			return err
		}
	}

	return nil
}

func inStrSlice(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func main() {
	_, err := flags.Parse(&opts)
	if err != nil {
		os.Exit(1)
	}

	err = dbOpen()
	if err != nil {
		log.Println(err)
		return
	}
	err = dbBootstrap()
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("DB Opened")

	srv := sugar.NewService(opts.Host, "sys.login.token", &sugar.ServiceOpts{Pulls: 10})
	srv.AddMethod("login", loginHandler)
	srv.AddMethod("otp", otpHandler)
	srv.AddMethod("create", createHandler)
	srv.AddMethod("consume", consumeHandler)
	srv.SetUser(opts.User)
	srv.SetPass(opts.Pass)

	err = srv.Serve()
	if err != nil {
		log.Println("Lost connection with nexus:", err)
	}
}

type LoginResponse struct {
	User string `json:"user"`
	Tags map[string]map[string]interface{}
}

func loginHandler(task *nxcore.Task) (interface{}, *nxcore.JsonRpcErr) {

	token := ei.N(task.Params).M("token").StringZ()

	ret, err := r.Table("tokens").
		Between(token, token+"\uffff").
		Filter(r.Row.Field("ttl").Ne(0)).
		Filter(r.Row.Field("deadline").During(r.Now(), r.Row.Field("deadline"), r.DuringOpts{RightBound: "closed"})).
		Update(r.Branch(r.Row.Field("ttl").Gt(0),
			ei.M{"ttl": r.Row.Field("ttl").Add(-1), "lastSeen": r.Now()},
			ei.M{"ttl": r.Row.Field("ttl"), "lastSeen": r.Now()}),
			r.UpdateOpts{ReturnChanges: true}).
		RunWrite(db)
	if err != nil {
		log.Println("Error:", err)
		return nil, &nxcore.JsonRpcErr{Cod: 1, Mess: "Internal Error"}
	}

	if len(ret.Changes) != 1 {
		return nil, &nxcore.JsonRpcErr{Cod: 2, Mess: "Invalid token"}
	}

	return ret.Changes[0].NewValue, nil
}

func otpHandler(task *nxcore.Task) (interface{}, *nxcore.JsonRpcErr) {
	log.Println("Creating OTP for", task.User)

	ret, err := r.Table("tokens").Insert(ei.M{"user": task.User, "ttl": 1, "deadline": r.Now().Add(3600)}).
		RunWrite(db)
	if err == nil && len(ret.GeneratedKeys) > 0 {
		return ret.GeneratedKeys[0], nil
	}

	return nil, &nxcore.JsonRpcErr{Cod: 3, Mess: err.Error()}
}

func createHandler(task *nxcore.Task) (interface{}, *nxcore.JsonRpcErr) {

	ttl := ei.N(task.Params).M("ttl").IntZ()
	if ttl == 0 {
		ttl = 1
	}

	deadline, err := ei.N(task.Params).M("deadline").Time()
	if err != nil {
		return nil, &nxcore.JsonRpcErr{Cod: 5, Mess: "Deadline conversion error"}
	}

	cur, err := r.Expr(r.Now()).Run(db)
	if err != nil {
		log.Println("Error:", err)
		return nil, &nxcore.JsonRpcErr{Cod: 1, Mess: "Internal Error"}
	}
	var t time.Time
	cur.One(&t)
	if deadline.Before(t) {
		return nil, &nxcore.JsonRpcErr{Cod: 4, Mess: "Deadline is in the past"}
	}

	ret, err := r.Table("tokens").Insert(ei.M{"user": task.User, "ttl": ttl, "deadline": deadline}).RunWrite(db)
	if err == nil && len(ret.GeneratedKeys) > 0 {
		log.Println("Creating token for", task.User)

		return ret.GeneratedKeys[0], nil
	}

	return nil, &nxcore.JsonRpcErr{Cod: 3, Mess: err.Error()}
}

func consumeHandler(task *nxcore.Task) (interface{}, *nxcore.JsonRpcErr) {

	token, err := ei.N(task.Params).M("token").String()
	if err != nil {
		return nil, &nxcore.JsonRpcErr{Cod: 2, Mess: "Invalid token"}
	}

	ret, err := r.Table("tokens").Get(token).
		Update(ei.M{"ttl": 0}, r.UpdateOpts{ReturnChanges: true}).RunWrite(db)

	if len(ret.Changes) != 1 {
		return nil, &nxcore.JsonRpcErr{Cod: 2, Mess: "Invalid token"}
	}

	return ret.Changes[0].NewValue, nil
}
