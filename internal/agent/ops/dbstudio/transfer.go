package dbstudio

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"golang.org/x/sys/unix"
)

// The site-tree folders Quarry reads and writes (SPEC §7).
const (
	exportsDir = "shared/exports"
	importsDir = "shared/imports"
)

// Transfer limits (SPEC §7).
const (
	defaultMaxFile = 16 << 30
	maxChunkRead   = 4 << 20
	maxChunkWrite  = 8 << 20
	maxCSVRecord   = 64 << 20
	csvBatchRows   = 500
	csvBatchBytes  = 4 << 20
	maxPlaceholder = 65535
)

// fileNameRe is a file name Quarry accepts in a request.
var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,150}$`)

// validFile checks a file name: the rule, and .sql or .csv.
func validFile(name string) error {
	low := strings.ToLower(name)
	if !fileNameRe.MatchString(name) || !(strings.HasSuffix(low, ".sql") || strings.HasSuffix(low, ".csv")) {
		return agentrpc.Invalid("a file name is letters, digits, . _ and -, ending in .sql or .csv")
	}
	return nil
}

func (o *Ops) maxFile() int64 {
	if o.MaxFileBytes > 0 {
		return o.MaxFileBytes
	}
	return defaultMaxFile
}

// ensureDir makes the folder rel (0750, the site user's) if missing and
// refuses anything there but a real folder.
func ensureDir(r *sitefs.Root, rel string) error {
	fi, err := r.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return r.MkdirAll(rel, 0o750)
	case err != nil:
		return err
	case !fi.IsDir():
		return agentrpc.Invalid("%s is not a real directory; refusing to follow it", rel)
	}
	return nil
}

// fileError classifies a site-tree error for the browser.
func fileError(err error) error {
	switch {
	case errors.Is(err, sitefs.ErrEscape):
		return agentrpc.Invalid("path escapes the site root")
	case errors.Is(err, sitefs.ErrSymlink), errors.Is(err, unix.ELOOP):
		return agentrpc.Invalid("the file is a symlink; refusing to follow it")
	case errors.Is(err, sitefs.ErrNotRegular):
		return agentrpc.Invalid("not a regular file")
	case errors.Is(err, fs.ErrNotExist):
		return agentrpc.NotFound("no such file")
	}
	return err
}

// errTooLarge is a file that passed the size limit.
var errTooLarge = errors.New("the file reached the size limit")

// capped is a writer that fails past n bytes.
type capped struct {
	w io.Writer
	n int64
}

func (c *capped) Write(p []byte) (int, error) {
	if int64(len(p)) > c.n {
		return 0, errTooLarge
	}
	c.n -= int64(len(p))
	return c.w.Write(p)
}

// newExport creates a new file in shared/exports named base plus the
// time, with a counter when the name is taken.
func (o *Ops) newExport(r *sitefs.Root, base, ext string) (*os.File, string, error) {
	if err := ensureDir(r, exportsDir); err != nil {
		return nil, "", err
	}
	stem := base + "-" + o.now().UTC().Format("20060102-150405")
	for i := 1; i <= 100; i++ {
		name := stem + ext
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := r.Create(exportsDir+"/"+name, 0o640)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fileError(err)
		}
	}
	return nil, "", agentrpc.Conflict("too many exports named %s", stem)
}

// Export writes the database (sql) or one table (csv) into a new file of
// shared/exports, streaming (SPEC §7).
func (o *Ops) Export(ctx context.Context, p rpc.DBStudioExportParams) (rpc.DBStudioExportResult, error) {
	if p.Format != "sql" && p.Format != "csv" {
		return rpc.DBStudioExportResult{}, agentrpc.Invalid("the format is sql or csv")
	}
	if p.Format == "csv" && len(p.Tables) != 1 {
		return rpc.DBStudioExportResult{}, agentrpc.Invalid("a CSV export is of one table")
	}
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, true, bulkTimeout)
	if err != nil {
		return rpc.DBStudioExportResult{}, err
	}
	defer s.Close()
	tables := make([]*table, 0, len(p.Tables))
	for _, name := range p.Tables {
		t, err := lookup(ctx, s, name)
		if err != nil {
			return rpc.DBStudioExportResult{}, err
		}
		if p.Format == "sql" && !host.ValidTableName(t.Name) {
			return rpc.DBStudioExportResult{}, agentrpc.Invalid("%q cannot be exported alone; export the whole database", t.Name)
		}
		tables = append(tables, t)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBStudioExportResult{}, err
	}
	defer r.Close()
	base, ext := s.Database(), ".sql"
	if p.Format == "csv" {
		base, ext = s.Database()+"-"+safeStem(tables[0].Name), ".csv"
	}
	f, name, err := o.newExport(r, base, ext)
	if err != nil {
		return rpc.DBStudioExportResult{}, err
	}
	fail := func(err error) (rpc.DBStudioExportResult, error) {
		f.Close()
		_ = r.Remove(exportsDir + "/" + name)
		if errors.Is(err, errTooLarge) {
			return rpc.DBStudioExportResult{}, agentrpc.Invalid("the export passed %d GiB and was stopped", o.maxFile()>>30)
		}
		return rpc.DBStudioExportResult{}, err
	}
	out := &capped{w: f, n: o.maxFile()}
	res := rpc.DBStudioExportResult{Name: name}
	if p.Format == "sql" {
		names := make([]string, 0, len(tables))
		for _, t := range tables {
			names = append(names, t.Name)
		}
		dump := func() error { return o.DB.Dump(ctx, s.Database(), out, names...) }
		if p.Login != "" {
			// As the signed-in user: the dump holds what it may read.
			login, err := o.credentials(p.Site, p.Login)
			if err != nil {
				return fail(err)
			}
			dump = func() error { return o.DB.DumpAsLogin(ctx, login, s.Database(), out, names...) }
		}
		if err := dump(); err != nil {
			return fail(err)
		}
	} else {
		n, err := writeCSV(ctx, s, tables[0], out)
		if err != nil {
			return fail(err)
		}
		res.Rows = n
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	fi, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = r.Remove(exportsDir + "/" + name)
		return rpc.DBStudioExportResult{}, err
	}
	res.SizeBytes = fi.Size()
	return res, nil
}

// safeStem keeps a table name usable in a file name.
func safeStem(name string) string {
	out := []rune{}
	for _, c := range name {
		if c < 128 && (c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
		if len(out) == 64 {
			break
		}
	}
	return string(out)
}

// csvNull is how CSV export and import write NULL.
const csvNull = `\N`

// writeCSV streams every row of t into w: a header, then the rows.
func writeCSV(ctx context.Context, s interface {
	Each(context.Context, string, []any, func([]string, [][]byte) error) error
}, t *table, w io.Writer) (int64, error) {
	bw := bufio.NewWriterSize(w, 1<<20)
	cw := csv.NewWriter(bw)
	header := make([]string, len(t.Columns))
	b := &stmt{}
	b.sql("SELECT ")
	for i, c := range t.Columns {
		header[i] = c.Name
		if i > 0 {
			b.sql(", ")
		}
		b.ident(c.Name)
	}
	b.sql(" FROM ").ident(t.Name)
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	var n int64
	record := make([]string, len(t.Columns))
	err := s.Each(ctx, b.text(), nil, func(_ []string, row [][]byte) error {
		for i, v := range row {
			if v == nil {
				record[i] = csvNull
			} else {
				record[i] = string(v)
			}
		}
		n++
		return cw.Write(record)
	})
	if err != nil {
		return 0, err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return 0, err
	}
	return n, bw.Flush()
}

// Files lists shared/exports and shared/imports, newest first.
func (o *Ops) Files(ctx context.Context, p rpc.DBStudioSiteParams) (rpc.DBStudioFilesResult, error) {
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBStudioFilesResult{}, err
	}
	defer r.Close()
	list := func(dir string) ([]rpc.DBStudioFile, error) {
		out := []rpc.DBStudioFile{}
		if fi, err := r.Lstat(dir); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
			return out, nil
		}
		infos, err := r.ReadDir(dir)
		if err != nil {
			return nil, fileError(err)
		}
		for _, fi := range infos {
			if fi.Mode().IsRegular() && validFile(fi.Name()) == nil {
				out = append(out, rpc.DBStudioFile{Name: fi.Name(), Size: fi.Size(), Modified: fi.ModTime().UTC()})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
		return out, nil
	}
	exports, err := list(exportsDir)
	if err != nil {
		return rpc.DBStudioFilesResult{}, err
	}
	imports, err := list(importsDir)
	if err != nil {
		return rpc.DBStudioFilesResult{}, err
	}
	return rpc.DBStudioFilesResult{Exports: exports, Imports: imports}, nil
}

// openRegular opens rel for reading, never through a symlink, and requires
// a regular file.
func openRegular(r *sitefs.Root, rel string) (*os.File, os.FileInfo, error) {
	f, err := r.OpenFile(rel, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fileError(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, agentrpc.Invalid("not a regular file")
	}
	return f, fi, nil
}

// ReadChunk reads part of an export, for a streamed download.
func (o *Ops) ReadChunk(ctx context.Context, p rpc.DBStudioChunkParams) (rpc.DBStudioChunkResult, error) {
	if err := validFile(p.Name); err != nil {
		return rpc.DBStudioChunkResult{}, err
	}
	if p.Length < 1 || p.Length > maxChunkRead || p.Offset < 0 {
		return rpc.DBStudioChunkResult{}, agentrpc.Invalid("read 1 byte to %d MiB from a non-negative offset", maxChunkRead>>20)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBStudioChunkResult{}, err
	}
	defer r.Close()
	f, fi, err := openRegular(r, exportsDir+"/"+p.Name)
	if err != nil {
		return rpc.DBStudioChunkResult{}, err
	}
	defer f.Close()
	if p.Offset > fi.Size() {
		return rpc.DBStudioChunkResult{}, agentrpc.Invalid("the offset is past the end of the file")
	}
	buf := make([]byte, min(int64(p.Length), fi.Size()-p.Offset))
	n, err := f.ReadAt(buf, p.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return rpc.DBStudioChunkResult{}, err
	}
	return rpc.DBStudioChunkResult{Data: buf[:n], Size: fi.Size()}, nil
}

// UploadChunk writes part of an upload into shared/imports: offset 0
// starts the file anew, any other offset must be the file's size.
func (o *Ops) UploadChunk(ctx context.Context, p rpc.DBStudioUploadParams) (rpc.DBStudioUploadResult, error) {
	if err := validFile(p.Name); err != nil {
		return rpc.DBStudioUploadResult{}, err
	}
	if len(p.Data) > maxChunkWrite || p.Offset < 0 {
		return rpc.DBStudioUploadResult{}, agentrpc.Invalid("at most %d MiB per chunk, from a non-negative offset", maxChunkWrite>>20)
	}
	if p.Offset+int64(len(p.Data)) > o.maxFile() {
		return rpc.DBStudioUploadResult{}, agentrpc.Invalid("an upload is at most %d GiB", o.maxFile()>>30)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBStudioUploadResult{}, err
	}
	defer r.Close()
	if err := ensureDir(r, importsDir); err != nil {
		return rpc.DBStudioUploadResult{}, err
	}
	rel := importsDir + "/" + p.Name
	var f *os.File
	if p.Offset == 0 {
		if err := r.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return rpc.DBStudioUploadResult{}, fileError(err)
		}
		if f, err = r.Create(rel, 0o640); err != nil {
			return rpc.DBStudioUploadResult{}, fileError(err)
		}
	} else {
		if f, err = r.OpenFile(rel, unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW, 0); err != nil {
			return rpc.DBStudioUploadResult{}, fileError(err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return rpc.DBStudioUploadResult{}, err
		}
		if !fi.Mode().IsRegular() || fi.Size() != p.Offset {
			f.Close()
			return rpc.DBStudioUploadResult{}, agentrpc.Conflict("the upload has %d bytes, not %d: start it again", fi.Size(), p.Offset)
		}
	}
	_, err = f.Write(p.Data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return rpc.DBStudioUploadResult{}, err
	}
	return rpc.DBStudioUploadResult{Size: p.Offset + int64(len(p.Data))}, nil
}

// DeleteFile removes one export or upload.
func (o *Ops) DeleteFile(ctx context.Context, p rpc.DBStudioDeleteParams) (map[string]string, error) {
	if err := validFile(p.Name); err != nil {
		return nil, err
	}
	dir := map[string]string{"exports": exportsDir, "imports": importsDir}[p.Dir]
	if dir == "" {
		return nil, agentrpc.Invalid("the folder is exports or imports")
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := r.Remove(dir + "/" + p.Name); err != nil {
		return nil, fileError(err)
	}
	return map[string]string{"deleted": p.Name}, nil
}

// recordGuard fails a read that has gone on for more than limit bytes
// since the last complete record: encoding/csv holds a record whole, so a
// file with one endless field must not be read into memory.
type recordGuard struct {
	r          io.Reader
	read, mark int64
	limit      int64
}

func (g *recordGuard) Read(p []byte) (int, error) {
	if g.read-g.mark > g.limit {
		return 0, agentrpc.Invalid("a CSV record is longer than %d MiB", g.limit>>20)
	}
	n, err := g.r.Read(p)
	g.read += int64(n)
	return n, err
}

// ImportCSV loads a CSV file of shared/imports into a table, all rows in
// one transaction (SPEC §7).
func (o *Ops) ImportCSV(ctx context.Context, p rpc.DBStudioImportCSVParams) (rpc.DBStudioImportCSVResult, error) {
	if err := validFile(p.Name); err != nil {
		return rpc.DBStudioImportCSVResult{}, err
	}
	if !strings.HasSuffix(strings.ToLower(p.Name), ".csv") {
		return rpc.DBStudioImportCSVResult{}, agentrpc.Invalid("a CSV import reads a .csv file")
	}
	s, err := o.sessionFor(ctx, p.Site, p.Login, p.Database, p.ReadOnly, bulkTimeout)
	if err != nil {
		return rpc.DBStudioImportCSVResult{}, err
	}
	defer s.Close()
	t, err := lookup(ctx, s, p.Table)
	if err != nil {
		return rpc.DBStudioImportCSVResult{}, err
	}
	if t.Type != "table" {
		return rpc.DBStudioImportCSVResult{}, agentrpc.Invalid("%s is a view", t.Name)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.DBStudioImportCSVResult{}, err
	}
	f, _, err := openRegular(r, importsDir+"/"+p.Name)
	r.Close()
	if err != nil {
		return rpc.DBStudioImportCSVResult{}, err
	}
	defer f.Close()
	guard := &recordGuard{r: f, limit: maxCSVRecord}
	cr := csv.NewReader(bufio.NewReaderSize(guard, 1<<20))
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err != nil {
		return rpc.DBStudioImportCSVResult{}, agentrpc.Invalid("read the CSV header: %v", csvMessage(err))
	}
	guard.mark = guard.read
	cols := make([]rpc.DBColumn, len(header))
	seen := map[string]bool{}
	for i, h := range header {
		c, err := t.column(strings.TrimPrefix(h, "\ufeff"))
		if err != nil {
			return rpc.DBStudioImportCSVResult{}, err
		}
		if c.Binary {
			return rpc.DBStudioImportCSVResult{}, agentrpc.Invalid("%s is a binary column; it cannot be imported from CSV", c.Name)
		}
		if seen[c.Name] {
			return rpc.DBStudioImportCSVResult{}, agentrpc.Invalid("the column %s is named twice", c.Name)
		}
		seen[c.Name] = true
		cols[i] = c
	}
	perBatch := min(csvBatchRows, maxPlaceholder/len(cols))
	if _, err := s.Exec(ctx, "START TRANSACTION"); err != nil {
		return rpc.DBStudioImportCSVResult{}, sqlErr(s, err)
	}
	var total int64
	var batch []any
	rows, bytes := 0, 0
	flush := func() error {
		if rows == 0 {
			return nil
		}
		b := &stmt{}
		b.sql("INSERT INTO ").ident(t.Name).sql(" (")
		for i, c := range cols {
			if i > 0 {
				b.sql(", ")
			}
			b.ident(c.Name)
		}
		b.sql(") VALUES ")
		one := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
		q := b.text() + strings.TrimSuffix(strings.Repeat(one+", ", rows), ", ")
		n, err := s.Exec(ctx, q, batch...)
		if err != nil {
			return sqlErr(s, err)
		}
		total += n
		batch, rows, bytes = batch[:0], 0, 0
		return nil
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return rpc.DBStudioImportCSVResult{}, rollback(ctx, s, agentrpc.Invalid("read the CSV file: %v", csvMessage(err)))
		}
		guard.mark = guard.read
		for _, v := range rec {
			if v == csvNull {
				batch = append(batch, nil)
			} else {
				batch = append(batch, v)
			}
			bytes += len(v)
		}
		rows++
		if rows >= perBatch || bytes >= csvBatchBytes {
			if err := flush(); err != nil {
				return rpc.DBStudioImportCSVResult{}, rollback(ctx, s, err)
			}
		}
	}
	if err := flush(); err != nil {
		return rpc.DBStudioImportCSVResult{}, rollback(ctx, s, err)
	}
	if _, err := s.Exec(ctx, "COMMIT"); err != nil {
		return rpc.DBStudioImportCSVResult{}, rollback(ctx, s, sqlErr(s, err))
	}
	return rpc.DBStudioImportCSVResult{Rows: total}, nil
}

// csvMessage is a CSV error without the reader's internals; a guard's
// refusal stays as it is.
func csvMessage(err error) string {
	var ae *agentrpc.Error
	if errors.As(err, &ae) {
		return ae.Message
	}
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fmt.Sprintf("line %d: %v", pe.Line, pe.Err)
	}
	return "the file could not be read"
}
