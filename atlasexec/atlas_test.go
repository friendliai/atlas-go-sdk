package atlasexec_test

import (
	"ariga.io/atlas/sql/migrate"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/cmd/atlas/x"
	"ariga.io/atlas/sql/sqlcheck"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func Test_MigrateApply(t *testing.T) {
	ec, err := atlasexec.NewWorkingDir(
		atlasexec.WithMigrations(os.DirFS(filepath.Join("testdata", "migrations"))),
		atlasexec.WithAtlasHCL(func(w io.Writer) error {
			_, err := w.Write([]byte(`
			variable "url" {
				type    = string
				default = getenv("DB_URL")
			}
			env {
				name = atlas.env
				url  = var.url
				migration {
					dir = "file://migrations"
				}
			}`))
			return err
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, ec.Close())
	})
	c, err := atlasexec.NewClient(ec.Path(), "atlas")
	require.NoError(t, err)
	got, err := c.MigrateApply(context.Background(), &atlasexec.MigrateApplyParams{
		Env: "test",
	})
	require.EqualError(t, err, `atlasexec: required flag "url" not set`)
	require.Nil(t, got)
	// Set the env var and try again
	os.Setenv("DB_URL", "sqlite://file?_fk=1&cache=shared&mode=memory")
	got, err = c.MigrateApply(context.Background(), &atlasexec.MigrateApplyParams{
		Env: "test",
	})
	require.NoError(t, err)
	require.EqualValues(t, "20230926085734", got.Target)
}

func TestMigrateLint(t *testing.T) {
	t.Run("with broken config", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		got, err := c.MigrateLint(context.Background(), &atlasexec.MigrateLintParams{
			ConfigURL: "file://config-broken.hcl",
		})
		require.ErrorContains(t, err, `project file "config-broken.hcl" was not found`)
		require.Nil(t, got)
	})
	t.Run("with broken dev-url", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		got, err := c.MigrateLint(context.Background(), &atlasexec.MigrateLintParams{
			DirURL: "file://atlasexec/testdata/migrations",
		})
		require.ErrorContains(t, err, `required flag(s) "dev-url" not set`)
		require.Nil(t, got)
	})
	t.Run("broken dir", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		got, err := c.MigrateLint(context.Background(), &atlasexec.MigrateLintParams{
			DevURL: "sqlite://file?mode=memory",
			DirURL: "file://atlasexec/testdata/doesnotexist",
		})
		require.ErrorContains(t, err, `stat atlasexec/testdata/doesnotexist: no such file or directory`)
		require.Nil(t, got)
	})
	t.Run("lint error parsing", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		got, err := c.MigrateLint(context.Background(), &atlasexec.MigrateLintParams{
			DevURL: "sqlite://file?mode=memory",
			DirURL: "file://testdata/migrations",
			Latest: 1,
		})
		require.NoError(t, err)
		require.GreaterOrEqual(t, 4, len(got.Steps))
		require.Equal(t, "sqlite3", got.Env.Driver)
		require.Equal(t, "testdata/migrations", got.Env.Dir)
		require.Equal(t, "sqlite://file?mode=memory", got.Env.URL.String())
		require.Equal(t, 1, len(got.Files))
		expectedReport := &x.FileReport{
			Name: "20230926085734_destructive-change.sql",
			Text: "DROP TABLE t2;\n",
			Reports: []sqlcheck.Report{{
				Text: "destructive changes detected",
				Diagnostics: []sqlcheck.Diagnostic{{
					Pos:  0,
					Text: `Dropping table "t2"`,
					Code: "DS102",
				}},
			}},
			Error: "destructive changes detected",
		}
		require.EqualValues(t, expectedReport, got.Files[0])
	})
	t.Run("lint with manually parsing output", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		var buf bytes.Buffer
		err = c.MigrateLintError(context.Background(), &atlasexec.MigrateLintParams{
			DevURL: "sqlite://file?mode=memory",
			DirURL: "file://testdata/migrations",
			Latest: 1,
			Writer: &buf,
		})
		require.NoError(t, err)
		var raw json.RawMessage
		require.NoError(t, json.NewDecoder(&buf).Decode(&raw))
	})
	t.Run("lint uses --base and --latest", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		summary, err := c.MigrateLint(context.Background(), &atlasexec.MigrateLintParams{
			DevURL: "sqlite://file?mode=memory",
			DirURL: "file://testdata/migrations",

			Latest: 1,
			Base:   "atlas://test-dir",
		})
		require.ErrorContains(t, err, "--latest, --git-base, and --base are mutually exclusive")
		require.Nil(t, summary)
	})
}

func TestMigrateLintWithLogin(t *testing.T) {
	type graphQLQuery struct {
		Query     string          `json:"query"`
		Variables json.RawMessage `json:"variables"`
	}
	type Dir struct {
		Name    string `json:"name"`
		Content string `json:"content"`
		Slug    string `json:"slug"`
	}
	type dirsQueryResponse struct {
		Data struct {
			Dirs []Dir `json:"dirs"`
		} `json:"data"`
	}
	token := "123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
		var query graphQLQuery
		require.NoError(t, json.NewDecoder(r.Body).Decode(&query))
		switch {
		case strings.Contains(query.Query, "mutation reportMigrationLint"):
			_, _ = fmt.Fprintf(w, `{ "data": { "reportMigrationLint": { "url": "https://migration-lint-report-url" } } }`)
		case strings.Contains(query.Query, "query dirs"):
			dir, err := migrate.NewLocalDir("./testdata/migrations")
			require.NoError(t, err)
			ad, err := migrate.ArchiveDir(dir)
			require.NoError(t, err)
			var resp dirsQueryResponse
			resp.Data.Dirs = []Dir{{
				Name:    "test-dir-name",
				Slug:    "test-dir-slug",
				Content: base64.StdEncoding.EncodeToString(ad),
			}}
			st2bytes, err := json.Marshal(resp)
			require.NoError(t, err)
			_, _ = fmt.Fprint(w, string(st2bytes))
		}
	}))
	t.Cleanup(srv.Close)
	atlasConfigURL := generateHCL(t, srv.URL, token)
	t.Run("Web and Writer params produces an error", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		params := &atlasexec.MigrateLintParams{
			ConfigURL: atlasConfigURL,
			DevURL:    "sqlite://file?mode=memory",
			DirURL:    "file://testdata/migrations",
			Latest:    1,
			Web:       true,
		}
		got, err := c.MigrateLint(context.Background(), params)
		require.ErrorContains(t, err, "Writer or Web reporting are not supported")
		require.Nil(t, got)
		params.Web = false
		params.Writer = &bytes.Buffer{}
		got, err = c.MigrateLint(context.Background(), params)
		require.ErrorContains(t, err, "Writer or Web reporting are not supported")
		require.Nil(t, got)
	})
	t.Run("lint parse web output", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		var buf bytes.Buffer
		require.NoError(t, c.MigrateLintError(context.Background(), &atlasexec.MigrateLintParams{
			DevURL:    "sqlite://file?mode=memory",
			DirURL:    "file://testdata/migrations",
			ConfigURL: atlasConfigURL,
			Latest:    1,
			Writer:    &buf,
			Web:       true,
		}))
		require.Equal(t, strings.TrimSpace(buf.String()), "https://migration-lint-report-url")
	})
	t.Run("lint uses --base", func(t *testing.T) {
		c, err := atlasexec.NewClient(".", "atlas")
		require.NoError(t, err)
		summary, err := c.MigrateLint(context.Background(), &atlasexec.MigrateLintParams{
			DevURL: "sqlite://file?mode=memory",
			DirURL: "file://testdata/migrations",

			ConfigURL: atlasConfigURL,
			Base:      "atlas://test-dir-slug",
		})
		require.NoError(t, err)
		require.NotNil(t, summary)
	})
}

func generateHCL(t *testing.T, url, token string) string {
	tmpl := `atlas {
		cloud {
			token = "{{ .Token }}"
		{{- if .URL }}
			url = "{{ .URL }}"
		{{- end }}
		}	  
	}
	env "test" {}`
	config := template.Must(template.New("atlashcl").Parse(tmpl))
	templateParams := struct {
		URL   string
		Token string
	}{
		URL:   url,
		Token: token,
	}
	var buf bytes.Buffer
	require.NoError(t, config.Execute(&buf, templateParams))
	atlasConfigURL, clean, err := atlasexec.TempFile(buf.String(), "hcl")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, clean())
	})
	return atlasConfigURL
}

func testDir(t *testing.T, path string) (d migrate.MemDir) {
	rd, err := os.ReadDir(path)
	require.NoError(t, err)
	for _, f := range rd {
		fp := filepath.Join(path, f.Name())
		b, err := os.ReadFile(fp)
		require.NoError(t, err)
		require.NoError(t, d.WriteFile(f.Name(), b))
	}
	return d
}

func Test_MigrateStatus(t *testing.T) {
	type args struct {
		ctx  context.Context
		data *atlasexec.MigrateStatusParams
	}
	tests := []struct {
		name        string
		args        args
		wantCurrent string
		wantNext    string
		wantErr     bool
	}{
		{
			args: args{
				ctx: context.Background(),
				data: &atlasexec.MigrateStatusParams{
					DirURL: "file://testdata/migrations",
				},
			},
			wantCurrent: "No migration applied yet",
			wantNext:    "20230727105553",
		},
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	c, err := atlasexec.NewClient(wd, "atlas")
	require.NoError(t, err)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbpath := sqlitedb(t)
			path := fmt.Sprintf("sqlite://%s", dbpath)
			tt.args.data.URL = path
			got, err := c.MigrateStatus(tt.args.ctx, tt.args.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("migrateStatus() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Equal(t, tt.wantCurrent, got.Current)
			require.Equal(t, tt.wantNext, got.Next)
		})
	}
}

func Test_SchemaApply(t *testing.T) {
	ce, err := atlasexec.NewWorkingDir()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, ce.Close())
	})
	f, err := os.CreateTemp("", "sqlite-test")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	u := fmt.Sprintf("sqlite://%s?_fk=1", f.Name())
	c, err := atlasexec.NewClient(ce.Path(), "atlas")
	require.NoError(t, err)

	s1 := `
	-- create table "users
	CREATE TABLE users(
		id int NOT NULL,
		name varchar(100) NULL,
		PRIMARY KEY(id)
	);`
	path, err := ce.WriteFile("schema.sql", []byte(s1))
	to := fmt.Sprintf("file://%s", path)
	require.NoError(t, err)
	_, err = c.SchemaApply(context.Background(), &atlasexec.SchemaApplyParams{
		URL:    u,
		To:     to,
		DevURL: "sqlite://file?_fk=1&cache=shared&mode=memory",
	})
	require.NoError(t, err)
	_, err = ce.WriteFile("schema.sql", []byte(s1+`
	-- create table "blog_posts"
	CREATE TABLE blog_posts(
		id int NOT NULL,
		title varchar(100) NULL,
		body text NULL,
		author_id int NULL,
		PRIMARY KEY(id),
		CONSTRAINT author_fk FOREIGN KEY(author_id) REFERENCES users(id)
	);`))
	require.NoError(t, err)
	_, err = c.SchemaApply(context.Background(), &atlasexec.SchemaApplyParams{
		URL:    u,
		To:     to,
		DevURL: "sqlite://file?_fk=1&cache=shared&mode=memory",
	})
	require.NoError(t, err)

	s, err := c.SchemaInspect(context.Background(), &atlasexec.SchemaInspectParams{
		URL: u,
	})
	require.NoError(t, err)
	require.Equal(t, `table "users" {
  schema = schema.main
  column "id" {
    null = false
    type = int
  }
  column "name" {
    null = true
    type = varchar
  }
  primary_key {
    columns = [column.id]
  }
}
table "blog_posts" {
  schema = schema.main
  column "id" {
    null = false
    type = int
  }
  column "title" {
    null = true
    type = varchar
  }
  column "body" {
    null = true
    type = text
  }
  column "author_id" {
    null = true
    type = int
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "author_fk" {
    columns     = [column.author_id]
    ref_columns = [table.users.column.id]
    on_update   = NO_ACTION
    on_delete   = NO_ACTION
  }
}
schema "main" {
}
`, s)
}

func TestVersion(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	c, err := atlasexec.NewClient(t.TempDir(), filepath.Join(wd, "./mock-atlas.sh"))
	require.NoError(t, err)

	for _, tt := range []struct {
		env    string
		expect *atlasexec.Version
	}{
		{
			env:    "",
			expect: &atlasexec.Version{Version: "1.2.3"},
		},
		{
			env: "v0.14.1-abcdef-canary",
			expect: &atlasexec.Version{
				Version: "0.14.1",
				SHA:     "abcdef",
				Canary:  true,
			},
		},
		{
			env: "v11.22.33-sha",
			expect: &atlasexec.Version{
				Version: "11.22.33",
				SHA:     "sha",
			},
		},
	} {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("TEST_ATLAS_VERSION", tt.env)
			v, err := c.Version(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.expect, v)
		})
	}
}

func sqlitedb(t *testing.T) string {
	td := t.TempDir()
	dbpath := filepath.Join(td, "file.db")
	_, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared&_fk=1", dbpath))
	require.NoError(t, err)
	return dbpath
}
