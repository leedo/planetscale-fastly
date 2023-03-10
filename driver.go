package planetscale

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/valyala/fastjson"
)

const (
	apiPrefix        = "/psdb.v1alpha1.Database"
	executorEndpoint = apiPrefix + "/Execute"
	sessionEndpoint  = apiPrefix + "/CreateSession"
	executorMethod   = "POST"
	jsonContentType  = "application/json"
	userAgent        = "database-go"
)

var unknownError = fmt.Errorf("unknown error")

type PsDriver struct{}

type PsConn struct {
	username string
	password string
	host     string
	backend  string
	session  []byte
}

type PsField struct {
	Name         string
	Type         string
	Table        string
	ColumnLength uint
	Charset      uint
	Flags        uint
}

type PsRow struct {
	Values [][]byte
}

type PsResults struct {
	Fields []PsField
	Rows   []PsRow
	pos    int
}

func (d PsDriver) Open(dsn string) (driver.Conn, error) {
	m, err := url.ParseQuery(dsn)
	if err != nil {
		return nil, fmt.Errorf("error parsing dsn: %w", err)
	}

	return PsConn{
		username: m.Get("username"),
		password: m.Get("password"),
		host:     m.Get("host"),
		backend:  m.Get("backend"),
	}, nil
}

func (c PsConn) Close() error {
	c.session = nil
	return nil
}

func (c PsConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("Prepare method not implemented")
}

func (c PsConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("Begin method not implemented")
}

func (c *PsConn) Rollback() (driver.Stmt, error) {
	return nil, fmt.Errorf("Rollback method not implemented")
}

func (c *PsConn) buildRequest(endpoint string, body []byte) (*fsthttp.Request, error) {
	u := "https://" + c.host + endpoint

	req, err := fsthttp.NewRequest(executorMethod, u, nil)
	if err != nil {
		return nil, err
	}

	req.Body = io.NopCloser(bytes.NewReader(body))

	auth := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
	req.Header.Add("Host", c.host)
	req.Header.Add("Content-Type", jsonContentType)
	req.Header.Add("User-Agent", userAgent)
	req.Header.Add("Authorization", "Basic "+auth)

	return req, nil
}

func (c *PsConn) sendRequest(ctx context.Context, req *fsthttp.Request) ([]byte, error) {
	resp, err := req.Send(ctx, c.backend)
	if err != nil {
		return nil, err
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("planetscale API error reading response body: %s", err)
	}

	if resp.StatusCode != fsthttp.StatusOK {

		return nil, fmt.Errorf("planetscale API error: %d\n%s", resp.StatusCode, respBody)
	}

	return respBody, nil
}

func (c *PsConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return c.QueryContext(context.Background(), query, args)
}

func (c *PsConn) readFields(f *fastjson.Value) ([]PsField, error) {
	if f == nil {
		return nil, fmt.Errorf("missing fields")
	}

	var fields []PsField
	for _, v := range f.GetArray() {
		fields = append(fields, PsField{
			Name:         string(v.GetStringBytes("name")),
			Type:         string(v.GetStringBytes("type")),
			Table:        string(v.GetStringBytes("table")),
			ColumnLength: v.GetUint("columnLength"),
			Charset:      v.GetUint("charset"),
			Flags:        v.GetUint("flags"),
		})
	}

	return fields, nil
}

func (c *PsConn) readRows(v *fastjson.Value) ([]PsRow, error) {
	if v == nil {
		return nil, fmt.Errorf("missing rows")
	}

	r := v.GetArray()
	rows := make([]PsRow, len(r))

	for i, v := range r {
		b := v.GetStringBytes("values")
		dst := make([]byte, base64.StdEncoding.DecodedLen(len(b)))
		n, err := base64.StdEncoding.Decode(dst, b)
		if err != nil {
			return nil, err
		}
		dst = dst[:n]

		lengths := v.GetArray("lengths")
		row := PsRow{make([][]byte, len(lengths))}

		var pos uint64
		for i, l := range lengths {
			val := string(l.GetStringBytes())
			u, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return nil, err
			}
			row.Values[i] = dst[pos : pos+u]
			pos += u
		}

		rows[i] = row
	}

	return rows, nil
}

func (c *PsConn) refreshSession(ctx context.Context) error {
	req, err := c.buildRequest(sessionEndpoint, []byte("{}"))
	if err != nil {
		return err
	}

	respBody, err := c.sendRequest(ctx, req)
	if err != nil {
		return err
	}

	var p fastjson.Parser
	v, err := p.ParseBytes(respBody)

	c.session = []byte{}
	c.session = v.GetObject("session").MarshalTo(c.session)
	return nil
}

func (c *PsConn) QueryContext(ctx context.Context, query string, args []driver.Value) (driver.Rows, error) {
	if c.session == nil {
		if err := c.refreshSession(ctx); err != nil {
			return nil, err
		}
	}

	q, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}

	body := []byte(`{"query":`)
	body = append(body, q[:]...)
	body = append(body, []byte(`,"session":`)...)
	body = append(body, c.session[:]...)
	body = append(body, []byte(`}`)...)

	req, err := c.buildRequest(executorEndpoint, body)
	if err != nil {
		return nil, err
	}

	resp, err := c.sendRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	var p fastjson.Parser
	v, err := p.ParseBytes(resp)
	if err != nil {
		return nil, err
	}

	if session := v.GetObject("session"); session != nil {
		c.session = []byte{}
		c.session = session.MarshalTo(c.session)
	}

	if jsonErr := v.GetObject("error"); jsonErr != nil {
		if msg := jsonErr.Get("message"); msg != nil {
			return nil, fmt.Errorf("%s", msg.GetStringBytes())
		}
		return nil, unknownError
	}

	result := v.GetObject("result")
	if result == nil {
		return nil, fmt.Errorf("no result")
	}

	f, err := c.readFields(result.Get("fields"))
	if err != nil {
		return nil, err
	}

	r, err := c.readRows(result.Get("rows"))
	if err != nil {
		return nil, err
	}

	results := &PsResults{Fields: f, Rows: r}
	return results, nil
}

func (r *PsResults) Columns() []string {
	var cols []string
	for _, f := range r.Fields {
		cols = append(cols, f.Name)
	}
	return cols
}

func (r *PsResults) Close() error {
	return nil
}

func (r *PsResults) Next(dest []driver.Value) error {
	if r.pos+1 > len(r.Rows) {
		return io.EOF
	}

	row := r.Rows[r.pos]

	for i := 0; i != len(row.Values); i++ {
		dest[i] = row.Values[i]
	}

	r.pos++
	return nil
}

func init() {
	sql.Register("planetscale", &PsDriver{})
}
