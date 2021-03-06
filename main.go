package main

import (
	"log"
	"os"
	"time"

	r "github.com/dancannon/gorethink"
	"github.com/jaracil/ei"
	"github.com/jessevdk/go-flags"
	"github.com/nayarsystems/nxsugar-go"
)

var opts struct {
	Config     string `short:"c" default:"config.json" description:"nexus config file"`
	Production bool   `long:"production" description:"Log as json"`

	Rethink RethinkOptions `group:"RethinkDB Options"`
}

type RethinkOptions struct {
	Host     []string `short:"r" long:"rethinkdb" description:"RethinkDB host[:port]" default:"localhost:28015"`
	Database string   `long:"db" description:"RethinkDB database" default:"nexusTokenAuth"`
	User     string   `long:"ruser" description:"RethinkDB username" default:""`
	Pass     string   `long:"rpass" description:"RethinkDB password" default:""`
}

var (
	db  *r.Session
	srv *nxsugar.Service
)

func dbOpen() (err error) {
	db, err = r.Connect(r.ConnectOpts{
		Addresses: opts.Rethink.Host,
		Database:  opts.Rethink.Database,
		MaxIdle:   50,
		MaxOpen:   200,
		Username:  opts.Rethink.User,
		Password:  opts.Rethink.Pass,
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

	nxsugar.SetFlagsEnabled(false)
	nxsugar.SetConfigFile(opts.Config)
	nxsugar.SetProductionMode(opts.Production)
	srv, err = nxsugar.NewServiceFromConfig("token-auth")
	if err != nil {
		log.Fatalln(err)
	}
	srv.AddMethod("login", loginHandler)
	srv.AddMethod("otp", otpHandler)
	srv.AddMethod("create", createHandler)
	srv.AddMethod("consume", consumeHandler)
	srv.AddMethod("list", listHandler)
	srv.AddMethod("info", infoHandler)
	srv.AddMethod("clear", clearHandler)

	go deleteExpiredTokensDaily()

	err = srv.Serve()
	if err != nil {
		log.Println("Lost connection with nexus:", err)
	}
}

type LoginResponse struct {
	User string `json:"user"`
	Tags map[string]map[string]interface{}
}

func loginHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {

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
		return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}

	if len(ret.Changes) != 1 {
		return nil, &nxsugar.JsonRpcErr{Cod: 2, Mess: "Invalid token"}
	}

	return ret.Changes[0].NewValue, nil
}

func otpHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {
	log.Println("Creating OTP for", task.User)

	ret, err := r.Table("tokens").Insert(ei.M{"user": task.User, "ttl": 1, "deadline": r.Now().Add(3600)}).
		RunWrite(db)
	if err == nil && len(ret.GeneratedKeys) > 0 {
		return ret.GeneratedKeys[0], nil
	}

	return nil, &nxsugar.JsonRpcErr{Cod: 3, Mess: err.Error()}
}

func createHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {

	ttl := ei.N(task.Params).M("ttl").IntZ()
	if ttl == 0 {
		ttl = 1
	}

	deadline, err := ei.N(task.Params).M("deadline").Time()
	if err != nil {
		return nil, &nxsugar.JsonRpcErr{Cod: 5, Mess: "Deadline conversion error"}
	}

	cur, err := r.Expr(r.Now()).Run(db)
	if err != nil {
		log.Println("Error:", err)
		return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}
	var t time.Time
	cur.One(&t)
	if deadline.Before(t) {
		return nil, &nxsugar.JsonRpcErr{Cod: 4, Mess: "Deadline is in the past"}
	}

	user := task.User
	userToImpersonate := ei.N(task.Params).M("user_to_impersonate").StringZ()

	if userToImpersonate != "" {
		response, err := task.GetConn().UserGetEffectiveTags(user, userToImpersonate)
		if err != nil {
			return nil, &nxsugar.JsonRpcErr{Cod: 3, Mess: err.Error()}
		}
		isAdmin, err := ei.N(response).M("tags").M("@admin").Bool()
		if err != nil {
			return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
		}
		if isAdmin == true {
			user = userToImpersonate
		} else {
			return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrPermissionDenied}
		}
	}

	metadata := ei.N(task.Params).M("metadata").RawZ()
	ret, err := r.Table("tokens").Insert(ei.M{"user": user, "ttl": ttl, "deadline": deadline, "metadata": metadata}).RunWrite(db)
	if err == nil && len(ret.GeneratedKeys) > 0 {
		log.Println("Creating token for", user)

		return ret.GeneratedKeys[0], nil
	}

	return nil, &nxsugar.JsonRpcErr{Cod: 3, Mess: err.Error()}
}

func consumeHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {

	token, err := ei.N(task.Params).M("token").String()
	if err != nil {
		return nil, &nxsugar.JsonRpcErr{Cod: 2, Mess: "Invalid token"}
	}

	ret, err := r.Table("tokens").Get(token).
		Delete(r.DeleteOpts{ReturnChanges: true}).RunWrite(db)

	if len(ret.Changes) != 1 {
		return nil, &nxsugar.JsonRpcErr{Cod: 2, Mess: "Invalid token"}
	}

	return ret.Changes[0].NewValue, nil
}

func listHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {

	user := task.User
	stmt := r.Table("tokens")

	if path := ei.N(task.Params).M("path").StringZ(); path != "" {
		tags, err := task.GetConn().UserGetEffectiveTags(user, path)
		if err != nil {
			log.Println("Error: ", err)
			return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrPermissionDenied}
		}

		if ei.N(tags).M("tags").M("@admin").BoolZ() || ei.N(tags).M("tags").M("@sys.login.token.list").BoolZ() {
			stmt = stmt.Filter(r.Row.Field("user").Match("^" + path + "($|.)"))
		} else {
			return nil, nil
		}
	} else {
		stmt = stmt.Filter(r.Row.Field("user").Eq(user))
	}

	res, err := stmt.Run(db)
	if err != nil {
		log.Println("Error: ", err)
		return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}
	defer res.Close()
	var tokens []interface{}
	if err := res.All(&tokens); err != nil {
		log.Println("Error getting query results: ", err)
		return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}

	return tokens, nil
}

func infoHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {
	ids := ei.N(task.Params).M("ids").SliceZ()

	res, err := r.Table("tokens").
		GetAll(ids...).Run(db)
	if err != nil {
		log.Println("Error: ", err)
		return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}
	defer res.Close()

	var tokensInfo []interface{}
	if err := res.All(&tokensInfo); err != nil {
		log.Println("Error getting query results: ", err)
		return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}

	user := task.User

	for _, token := range tokensInfo {
		path := ei.N(token).M("user").StringZ()
		if path != user {
			tags, err := task.GetConn().UserGetEffectiveTags(user, path)
			if err != nil {
				log.Println("Error getting effective tags: ", err)
				return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInvalidParams}
			}
			if !ei.N(tags).M("tags").M("@admin").BoolZ() && !ei.N(tags).M("tags").M("@token.list").BoolZ() {
				log.Println("Error parsing tags: ", err)
				return nil, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInvalidParams}
			}
		}
	}

	return tokensInfo, nil
}

func clearHandler(task *nxsugar.Task) (interface{}, *nxsugar.JsonRpcErr) {
	return deleteExpiredTokens()
}

func deleteExpiredTokensDaily() {
	t := time.NewTicker(24 * time.Hour)
	for range t.C {
		deleteExpiredTokens()
	}
}

func deleteExpiredTokens() (int, *nxsugar.JsonRpcErr) {
	countTokensDeleted := 0
	ret, err := r.Table("tokens").Filter(r.Row.Field("ttl").Eq(0)).Delete(r.DeleteOpts{ReturnChanges: true}).RunWrite(db)
	if err != nil {
		srv.Log(nxsugar.ErrorLevel, "Error deleting tokens with ttl=0. %v", err)
		return 0, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}
	countTokensDeleted += len(ret.Changes)
	srv.Log(nxsugar.ErrorLevel, "Tokens with no more ttl deleted: %v", countTokensDeleted)

	ret, err = r.Table("tokens").
		Filter(r.Row.Field("deadline").Lt(r.Now())).
		Delete(r.DeleteOpts{ReturnChanges: true}).RunWrite(db)
	if err != nil {
		srv.Log(nxsugar.ErrorLevel, "Error deleting expired tokens. %v", err)
		return 0, &nxsugar.JsonRpcErr{Cod: nxsugar.ErrInternal}
	}
	countTokensDeleted += len(ret.Changes)
	srv.Log(nxsugar.ErrorLevel, "Tokens expired deleted: %v", countTokensDeleted)
	return countTokensDeleted, nil
}
