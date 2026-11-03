package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"listingboard/internal/board"
)

func TestStartMarksTheJobSuccessful(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != board.StatusRunning {
		t.Fatalf("status %s, want %s", got.Status, board.StatusRunning)
	}
	if got.RunID == "" {
		t.Fatal("runId is empty")
	}
	if len(got.Checklist) == 0 {
		t.Fatal("checklist is empty")
	}
	for _, step := range got.Checklist {
		if step.State != board.StepStatePending {
			t.Fatalf("step %q is %s, want pending", step.Label, step.State)
		}
	}
}

func TestUploadKeepsTheClientFilename(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")

	meRes := call(srv, http.MethodGet, "/api/me", cookie, nil, "")
	if meRes.Code != http.StatusOK {
		t.Fatalf("me status %d: %s", meRes.Code, meRes.Body.String())
	}
	var me account
	if err := json.Unmarshal(meRes.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}

	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	path := filepath.Join(dir, me.ID, job.ID, "body")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("brochure was not stored at %s: %v", path, err)
	}

	storedJob, err := srv.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("failed to retrieve job: %v", err)
	}
	if storedJob.Filename != "brochure.pdf" {
		t.Fatalf("expected job.Filename brochure.pdf, got %q", storedJob.Filename)
	}
}

func TestListHidesOtherUsersJobs(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs", grace, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var jobs []board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, item := range jobs {
		if item.ID == job.ID {
			t.Fatal("another account can see this sheet in the list")
		}
	}
}

func TestStartRequiresASession(t *testing.T) {
	srv, _ := newTestServer(t)
	res := call(srv, http.MethodPost, "/api/jobs/"+board.NewID()+"/start", "", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.Code)
	}
}

func TestUploadValidation(t *testing.T) {
	t.Run("ValidPDF", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader1@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Condo", "100 Pine St")

		pdfContent := []byte("%PDF-1.7\n%\xff\xff\x00\x00Binary stream content...")
		// Intentionally test with application/octet-stream to verify byte-level detection (%PDF-)
		res := uploadFileRaw(srv, cookie, job.ID, "brochure.pdf", pdfContent, "application/octet-stream")
		if res.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", res.Code, res.Body.String())
		}
		got := decodeJob(t, res)
		if got.Filename != "brochure.pdf" {
			t.Fatalf("expected filename brochure.pdf, got %q", got.Filename)
		}
		diskBody, err := os.ReadFile(filepath.Join(dir, userID, job.ID, "body"))
		if err != nil {
			t.Fatalf("failed to read stored brochure: %v", err)
		}
		if !bytes.Equal(diskBody, pdfContent) {
			t.Fatalf("stored bytes do not match uploaded bytes")
		}
	})

	t.Run("ValidTextUTF8", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader2@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Loft", "200 Oak St")

		textContent := []byte("Modern studio apartment with balcony. Price: €450,000. Contact: 📞 555-1234. UTF-8 verified.")
		res := uploadFileRaw(srv, cookie, job.ID, "listing.txt", textContent, "text/plain")
		if res.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", res.Code, res.Body.String())
		}
		got := decodeJob(t, res)
		if got.Filename != "listing.txt" {
			t.Fatalf("expected filename listing.txt, got %q", got.Filename)
		}
		diskBody, err := os.ReadFile(filepath.Join(dir, userID, job.ID, "body"))
		if err != nil {
			t.Fatalf("failed to read stored brochure: %v", err)
		}
		if !bytes.Equal(diskBody, textContent) {
			t.Fatalf("stored bytes do not match uploaded bytes")
		}
	})

	t.Run("InvalidBinaryFormat", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader3@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Villa", "300 Elm St")

		// PNG magic bytes - invalid binary (not PDF, not valid UTF-8)
		binaryData := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
		res := uploadFileRaw(srv, cookie, job.ID, "image.png", binaryData, "image/png")
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d: %s", res.Code, res.Body.String())
		}
		var errResp map[string]string
		if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("expected JSON error response: %v", err)
		}
		if errResp["error"] != "Brochure must be a PDF or a text file" {
			t.Fatalf("expected error 'Brochure must be a PDF or a text file', got %q", errResp["error"])
		}

		if _, err := os.Stat(filepath.Join(dir, userID, job.ID, "body")); !os.IsNotExist(err) {
			t.Fatal("file should not exist on disk after validation failure")
		}
		storedJob, _ := srv.store.Job(context.Background(), job.ID)
		if storedJob.Filename != "" {
			t.Fatalf("job.Filename should be empty, got %q", storedJob.Filename)
		}
	})

	t.Run("TextWithNULByte", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader4@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Cottage", "400 Maple St")

		// Text containing forbidden NUL byte
		dataWithNul := []byte("Valid text starting here...\x00...embedded null byte")
		res := uploadFileRaw(srv, cookie, job.ID, "invalid.txt", dataWithNul, "text/plain")
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d: %s", res.Code, res.Body.String())
		}
		var errResp map[string]string
		if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("expected JSON error response: %v", err)
		}
		if errResp["error"] != "Brochure must be a PDF or a text file" {
			t.Fatalf("expected error 'Brochure must be a PDF or a text file', got %q", errResp["error"])
		}

		if _, err := os.Stat(filepath.Join(dir, userID, job.ID, "body")); !os.IsNotExist(err) {
			t.Fatal("file should not exist on disk after validation failure")
		}
		storedJob, _ := srv.store.Job(context.Background(), job.ID)
		if storedJob.Filename != "" {
			t.Fatalf("job.Filename should be empty, got %q", storedJob.Filename)
		}
	})

	t.Run("EmptyFile", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader5@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Studio", "500 Walnut St")

		res := uploadFileRaw(srv, cookie, job.ID, "empty.txt", []byte{}, "text/plain")
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d: %s", res.Code, res.Body.String())
		}
		var errResp map[string]string
		if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("expected JSON error response: %v", err)
		}
		if errResp["error"] != "File is empty" {
			t.Fatalf("expected error 'File is empty', got %q", errResp["error"])
		}

		if _, err := os.Stat(filepath.Join(dir, userID, job.ID, "body")); !os.IsNotExist(err) {
			t.Fatal("file should not exist on disk after empty file upload")
		}
		storedJob, _ := srv.store.Job(context.Background(), job.ID)
		if storedJob.Filename != "" {
			t.Fatalf("job.Filename should be empty, got %q", storedJob.Filename)
		}
	})

	t.Run("FileOver1MiB", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader6@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Estate", "600 Birch St")

		// 1 MiB + 10 bytes starting with %PDF-
		largeData := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("A"), maxBrochureSize)...)
		res := uploadFileRaw(srv, cookie, job.ID, "toolarge.pdf", largeData, "application/pdf")
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d: %s", res.Code, res.Body.String())
		}
		var errResp map[string]string
		if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("expected JSON error response: %v", err)
		}
		if errResp["error"] != "File too large" {
			t.Fatalf("expected error 'File too large', got %q", errResp["error"])
		}

		if _, err := os.Stat(filepath.Join(dir, userID, job.ID, "body")); !os.IsNotExist(err) {
			t.Fatal("file should not exist on disk after oversized file upload")
		}
		storedJob, _ := srv.store.Job(context.Background(), job.ID)
		if storedJob.Filename != "" {
			t.Fatalf("job.Filename should be empty, got %q", storedJob.Filename)
		}
	})

	t.Run("DuplicateUpload", func(t *testing.T) {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "uploader7@example.com")
		userID := getUserID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Manor", "700 Cedar St")

		firstContent := []byte("%PDF-1.4 First Brochure")
		res1 := uploadFileRaw(srv, cookie, job.ID, "first.pdf", firstContent, "application/pdf")
		if res1.Code != http.StatusOK {
			t.Fatalf("first upload failed: %d: %s", res1.Code, res1.Body.String())
		}

		secondContent := []byte("%PDF-1.4 Second Brochure")
		res2 := uploadFileRaw(srv, cookie, job.ID, "second.pdf", secondContent, "application/pdf")
		if res2.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on duplicate upload, got %d: %s", res2.Code, res2.Body.String())
		}
		var errResp map[string]string
		if err := json.Unmarshal(res2.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("expected JSON error response: %v", err)
		}
		if errResp["error"] != "A brochure is already attached" {
			t.Fatalf("expected error 'A brochure is already attached', got %q", errResp["error"])
		}

		// Verify first brochure is still on disk unchanged
		diskBody, err := os.ReadFile(filepath.Join(dir, userID, job.ID, "body"))
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		if !bytes.Equal(diskBody, firstContent) {
			t.Fatal("stored brochure content was modified by duplicate upload attempt")
		}
		storedJob, _ := srv.store.Job(context.Background(), job.ID)
		if storedJob.Filename != "first.pdf" {
			t.Fatalf("expected job.Filename first.pdf, got %q", storedJob.Filename)
		}
	})
}

func TestUploadPathTraversalSafety(t *testing.T) {
	traversalFilenames := []string{
		"../../escaped.txt",
		"../../../../etc/passwd",
		`..\..\..\escaped_win.txt`,
		"/absolute/path/escaped.pdf",
		"../../../data/overwrite.txt",
	}

	for _, malName := range traversalFilenames {
		t.Run("Filename="+malName, func(t *testing.T) {
			srv, dir := newTestServer(t)
			cookie := signUp(t, srv, "traversal@example.com")
			userID := getUserID(t, srv, cookie)
			job := createSheet(t, srv, cookie, "Security House", "123 Defense St")

			content := []byte("%PDF-1.5 Safe content for traversal test")
			res := uploadFileRaw(srv, cookie, job.ID, malName, content, "application/pdf")
			if res.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d: %s", res.Code, res.Body.String())
			}

			// File MUST be stored strictly at dir/userID/jobID/body
			expectedPath := filepath.Join(dir, userID, job.ID, "body")
			storedBytes, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatalf("expected brochure at %s, error: %v", expectedPath, err)
			}
			if !bytes.Equal(storedBytes, content) {
				t.Fatal("stored content does not match uploaded content")
			}

			// Verify NO file was created anywhere outside {dir}/{userID}/{job.ID}/body
			var foundFiles []string
			_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					foundFiles = append(foundFiles, path)
				}
				return nil
			})
			if len(foundFiles) != 1 || foundFiles[0] != expectedPath {
				t.Fatalf("unexpected files found in data dir: %v (expected only %s)", foundFiles, expectedPath)
			}

			// Verify filename stored in job model keeps the original client filename
			storedJob, err := srv.store.Job(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedJob.Filename != malName {
				t.Fatalf("expected job.Filename to keep original %q, got %q", malName, storedJob.Filename)
			}

			// Verify downloading via GET /api/jobs/{id}/file works safely and serves the file
			downRes := call(srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, nil, "")
			if downRes.Code != http.StatusOK {
				t.Fatalf("download returned %d: %s", downRes.Code, downRes.Body.String())
			}
			if !bytes.Equal(downRes.Body.Bytes(), content) {
				t.Fatal("downloaded bytes do not match expected content")
			}
		})
	}
}

func TestUserIsolationReturns404(t *testing.T) {
	srv, _ := newTestServer(t)

	// User A (owner)
	userACookie := signUp(t, srv, "userA@example.com")
	jobA := createSheet(t, srv, userACookie, "User A Sheet", "100 A Road")
	uploadFile(t, srv, userACookie, jobA.ID, "docA.pdf", []byte("%PDF-1.4 User A content"), "application/pdf")

	// User B (attacker / foreign user)
	userBCookie := signUp(t, srv, "userB@example.com")

	nonexistentID := board.NewID()

	type endpointTest struct {
		name     string
		method   string
		makePath func(id string) string
		makeBody func() (io.Reader, string)
	}

	endpoints := []endpointTest{
		{
			name:   "GET /jobs/{id}",
			method: http.MethodGet,
			makePath: func(id string) string {
				return "/api/jobs/" + id
			},
			makeBody: func() (io.Reader, string) { return nil, "" },
		},
		{
			name:   "POST /jobs/{id}/file",
			method: http.MethodPost,
			makePath: func(id string) string {
				return "/api/jobs/" + id + "/file"
			},
			makeBody: func() (io.Reader, string) {
				var buf bytes.Buffer
				writer := multipart.NewWriter(&buf)
				header := textproto.MIMEHeader{}
				header.Set("Content-Disposition", `form-data; name="file"; filename="hack.pdf"`)
				header.Set("Content-Type", "application/pdf")
				part, _ := writer.CreatePart(header)
				_, _ = part.Write([]byte("%PDF-1.4 hack attempt"))
				_ = writer.Close()
				return &buf, writer.FormDataContentType()
			},
		},
		{
			name:   "POST /jobs/{id}/start",
			method: http.MethodPost,
			makePath: func(id string) string {
				return "/api/jobs/" + id + "/start"
			},
			makeBody: func() (io.Reader, string) { return nil, "" },
		},
		{
			name:   "GET /jobs/{id}/file",
			method: http.MethodGet,
			makePath: func(id string) string {
				return "/api/jobs/" + id + "/file"
			},
			makeBody: func() (io.Reader, string) { return nil, "" },
		},
	}

	// 1. Test Nonexistent Job ID with valid session
	for _, ep := range endpoints {
		t.Run("NonexistentID_"+ep.name, func(t *testing.T) {
			body, cType := ep.makeBody()
			res := call(srv, ep.method, ep.makePath(nonexistentID), userACookie, body, cType)
			if res.Code != http.StatusNotFound {
				t.Fatalf("expected 404 Not Found, got %d. Body: %s", res.Code, res.Body.String())
			}
			var errResp map[string]string
			if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("expected JSON response, got error %v: %s", err, res.Body.String())
			}
			if errResp["error"] != "Not found" {
				t.Fatalf("expected error message 'Not found', got %q", errResp["error"])
			}
		})
	}

	// 2. Test Foreign Job ID (User B accessing User A's job)
	for _, ep := range endpoints {
		t.Run("ForeignID_"+ep.name, func(t *testing.T) {
			body, cType := ep.makeBody()
			res := call(srv, ep.method, ep.makePath(jobA.ID), userBCookie, body, cType)
			if res.Code != http.StatusNotFound {
				t.Fatalf("expected 404 Not Found, got %d. Body: %s", res.Code, res.Body.String())
			}
			var errResp map[string]string
			if err := json.Unmarshal(res.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("expected JSON response, got error %v: %s", err, res.Body.String())
			}
			if errResp["error"] != "Not found" {
				t.Fatalf("expected error message 'Not found', got %q", errResp["error"])
			}
		})
	}

	// Verify User A's job was not touched
	jobACheck, err := srv.store.Job(context.Background(), jobA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if jobACheck.Status != board.StatusDraft {
		t.Fatalf("expected status draft, got %s", jobACheck.Status)
	}
	if jobACheck.Filename != "docA.pdf" {
		t.Fatalf("expected filename docA.pdf, got %s", jobACheck.Filename)
	}
}

func TestStartJobStateMachine(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "statemachine@example.com")
	job := createSheet(t, srv, cookie, "State Machine Loft", "100 State St")

	// 1. Initial draft state
	if job.Status != board.StatusDraft {
		t.Fatalf("initial status should be draft, got %q", job.Status)
	}
	if job.RunID != "" {
		t.Fatalf("initial runId should be empty, got %q", job.RunID)
	}
	for i, step := range job.Checklist {
		if step.State != board.StepStatePending {
			t.Fatalf("initial step %d (%q) state should be pending, got %q", i, step.Label, step.State)
		}
	}

	// 2. Start without brochure -> 400 "Attach a brochure first"
	resNoBrochure := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if resNoBrochure.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", resNoBrochure.Code, resNoBrochure.Body.String())
	}
	var errResp map[string]string
	_ = json.Unmarshal(resNoBrochure.Body.Bytes(), &errResp)
	if errResp["error"] != "Attach a brochure first" {
		t.Fatalf("expected error 'Attach a brochure first', got %q", errResp["error"])
	}

	// Verify job remains draft in store
	jobStored, err := srv.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if jobStored.Status != board.StatusDraft {
		t.Fatalf("expected draft status, got %q", jobStored.Status)
	}
	if jobStored.RunID != "" {
		t.Fatalf("expected empty runId, got %q", jobStored.RunID)
	}

	// 3. Attach brochure and start -> transitions to running
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4 Brochure"), "application/pdf")

	resStart := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if resStart.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", resStart.Code, resStart.Body.String())
	}
	startedJob := decodeJob(t, resStart)
	if startedJob.Status != board.StatusRunning {
		t.Fatalf("expected status running, got %q", startedJob.Status)
	}
	if startedJob.RunID == "" {
		t.Fatal("expected non-empty runId")
	}
	if len(startedJob.Checklist) != 3 {
		t.Fatalf("expected 3 checklist items, got %d", len(startedJob.Checklist))
	}
	for i, step := range startedJob.Checklist {
		if step.State != board.StepStatePending {
			t.Fatalf("checklist step %d (%q) is %q, MUST BE pending", i, step.Label, step.State)
		}
	}

	// Verify persisted state in store
	persistedJob, err := srv.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedJob.Status != board.StatusRunning {
		t.Fatalf("persisted status is %q, want running", persistedJob.Status)
	}
	if persistedJob.RunID != startedJob.RunID {
		t.Fatalf("persisted runId %q != returned runId %q", persistedJob.RunID, startedJob.RunID)
	}
	for i, step := range persistedJob.Checklist {
		if step.State != board.StepStatePending {
			t.Fatalf("persisted step %d state is %q, want pending", i, step.State)
		}
	}

	// 4. Start when already running -> 200 idempotent with same runId
	resIdempotent := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if resIdempotent.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on re-start, got %d: %s", resIdempotent.Code, resIdempotent.Body.String())
	}
	restartedJob := decodeJob(t, resIdempotent)
	if restartedJob.RunID != startedJob.RunID {
		t.Fatalf("expected same runId %q, got %q", startedJob.RunID, restartedJob.RunID)
	}
	if restartedJob.Status != board.StatusRunning {
		t.Fatalf("expected status running, got %q", restartedJob.Status)
	}
	for i, step := range restartedJob.Checklist {
		if step.State != board.StepStatePending {
			t.Fatalf("step %d state after re-start is %q, want pending", i, step.State)
		}
	}

	// 5. Start when another sheet for same user is running -> 409 Conflict
	job2 := createSheet(t, srv, cookie, "Second Sheet", "200 State St")
	uploadFile(t, srv, cookie, job2.ID, "brochure2.txt", []byte("Brochure 2 text content"), "text/plain")

	resConflict := call(srv, http.MethodPost, "/api/jobs/"+job2.ID+"/start", cookie, nil, "")
	if resConflict.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", resConflict.Code, resConflict.Body.String())
	}
	var conflictErr map[string]string
	_ = json.Unmarshal(resConflict.Body.Bytes(), &conflictErr)
	if conflictErr["error"] != "Another job is already running" {
		t.Fatalf("expected 'Another job is already running', got %q", conflictErr["error"])
	}
	job2Stored, err := srv.store.Job(context.Background(), job2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job2Stored.Status != board.StatusDraft {
		t.Fatalf("job2 status changed to %q, should remain draft", job2Stored.Status)
	}

	// 6. Another user starting their job while first user's job is running succeeds (no cross-user conflict)
	cookie2 := signUp(t, srv, "otheruser@example.com")
	jobOther := createSheet(t, srv, cookie2, "Other User Sheet", "300 Other Ave")
	uploadFile(t, srv, cookie2, jobOther.ID, "other.pdf", []byte("%PDF-1.4 other"), "application/pdf")

	resOtherStart := call(srv, http.MethodPost, "/api/jobs/"+jobOther.ID+"/start", cookie2, nil, "")
	if resOtherStart.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for other user, got %d: %s", resOtherStart.Code, resOtherStart.Body.String())
	}
	otherStarted := decodeJob(t, resOtherStart)
	if otherStarted.Status != board.StatusRunning {
		t.Fatalf("expected status running for other user, got %q", otherStarted.Status)
	}

	// 7. A failed sheet stays failed when /start is called
	storedOther, err := srv.store.Job(context.Background(), otherStarted.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedOther.Status = board.StatusFailed
	storedOther.Error = "Brochure is missing"
	if err := srv.store.UpdateJob(context.Background(), storedOther); err != nil {
		t.Fatal(err)
	}
	resFailedStart := call(srv, http.MethodPost, "/api/jobs/"+otherStarted.ID+"/start", cookie2, nil, "")
	if resFailedStart.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on failed sheet start, got %d: %s", resFailedStart.Code, resFailedStart.Body.String())
	}
	failedJob := decodeJob(t, resFailedStart)
	if failedJob.Status != board.StatusFailed {
		t.Fatalf("expected failed sheet to stay failed, got %q", failedJob.Status)
	}
	if failedJob.Error != "Brochure is missing" {
		t.Fatalf("expected error preserved, got %q", failedJob.Error)
	}
	if failedJob.RunID != otherStarted.RunID {
		t.Fatalf("expected runId preserved %q, got %q", otherStarted.RunID, failedJob.RunID)
	}
}

type trackingReader struct {
	r         io.Reader
	readBytes atomic.Int64
}

func (tr *trackingReader) Read(p []byte) (n int, err error) {
	n, err = tr.r.Read(p)
	tr.readBytes.Add(int64(n))
	return n, err
}

type repeatingReader struct {
	pattern []byte
	limit   int64
	offset  int64
}

func (rr *repeatingReader) Read(p []byte) (n int, err error) {
	if rr.offset >= rr.limit {
		return 0, io.EOF
	}
	rem := rr.limit - rr.offset
	toRead := int64(len(p))
	if toRead > rem {
		toRead = rem
	}
	for i := int64(0); i < toRead; i++ {
		p[i] = rr.pattern[(rr.offset+i)%int64(len(rr.pattern))]
	}
	rr.offset += toRead
	return int(toRead), nil
}

func TestUploadStreamingAndMemoryBounds(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "stream@example.com")
	userID := getUserID(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Stream Property", "100 Stream Ave")

	boundary := "----CustomBoundary12345"
	head := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"large.pdf\"\r\nContent-Type: application/pdf\r\n\r\n", boundary)
	tail := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	const streamFileSize = int64(2500000) // ~2.38 MiB (well above 1 MiB limit)
	fileBody := &repeatingReader{
		pattern: []byte("%PDF-1.4 large file chunk data padding bytes 1234567890\n"),
		limit:   streamFileSize,
	}

	fullStream := io.MultiReader(
		strings.NewReader(head),
		fileBody,
		strings.NewReader(tail),
	)

	tracker := &trackingReader{r: fullStream}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/"+job.ID+"/file", tracker)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// 1. Verify status is 400
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	// 2. Verify error message
	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("expected JSON error response, got error %v: %s", err, rec.Body.String())
	}
	if errResp["error"] != "File too large" {
		t.Fatalf("expected error 'File too large', got %q", errResp["error"])
	}

	// 3. Verify early termination and memory bounds:
	totalRead := tracker.readBytes.Load()
	t.Logf("Total bytes read from 2.5 MiB stream before server termination: %d bytes", totalRead)

	const maxAllowedRead = 1200000 // 1.2 MiB threshold
	if totalRead > maxAllowedRead {
		t.Fatalf("server buffered too much! Read %d bytes, expected <= %d bytes", totalRead, maxAllowedRead)
	}
	if totalRead < maxBrochureSize {
		t.Fatalf("server terminated prematurely before reading maxBrochureSize: read %d bytes", totalRead)
	}

	// 4. Verify no file exists on disk
	expectedPath := filepath.Join(dir, userID, job.ID, "body")
	if _, err := os.Stat(expectedPath); !os.IsNotExist(err) {
		t.Fatalf("expected no file at %s, but file exists or error: %v", expectedPath, err)
	}

	var filesFound []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			filesFound = append(filesFound, path)
		}
		return nil
	})
	if len(filesFound) > 0 {
		t.Fatalf("found unexpected files on disk in DATA_DIR: %v", filesFound)
	}

	// 5. Verify sheet in store still has empty filename
	storedJob, err := srv.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("failed to retrieve job from store: %v", err)
	}
	if storedJob.Filename != "" {
		t.Fatalf("job.Filename should be empty, got %q", storedJob.Filename)
	}
	if storedJob.Status != board.StatusDraft {
		t.Fatalf("job.Status should still be draft, got %q", storedJob.Status)
	}
}

func TestUnauthenticatedRequests(t *testing.T) {
	srv, _ := newTestServer(t)

	cookie := signUp(t, srv, "valid_user@example.com")
	job := createSheet(t, srv, cookie, "Auth Test House", "123 Security Blvd")
	jobID := job.ID

	type testRoute struct {
		method      string
		path        string
		body        string
		contentType string
	}

	routes := []testRoute{
		{method: http.MethodGet, path: "/api/me"},
		{method: http.MethodGet, path: "/api/jobs"},
		{method: http.MethodPost, path: "/api/jobs", body: `{"title":"test","address":"test"}`, contentType: "application/json"},
		{method: http.MethodGet, path: "/api/jobs/" + jobID},
		{method: http.MethodPost, path: "/api/jobs/" + jobID + "/file", body: "dummy", contentType: "multipart/form-data; boundary=xyz"},
		{method: http.MethodPost, path: "/api/jobs/" + jobID + "/start"},
		{method: http.MethodGet, path: "/api/jobs/" + jobID + "/file"},
	}

	type authCase struct {
		name       string
		cookieName string
		cookieVal  string
		sendCookie bool
	}

	authCases := []authCase{
		{name: "missing cookie", sendCookie: false},
		{name: "empty cookie value", sendCookie: true, cookieName: sessionCookie, cookieVal: ""},
		{name: "malformed/unknown session token", sendCookie: true, cookieName: sessionCookie, cookieVal: "nonexistent-token-xyz-12345"},
		{name: "wrong cookie name", sendCookie: true, cookieName: "wrong_cookie", cookieVal: "some-value"},
	}

	for _, route := range routes {
		for _, ac := range authCases {
			t.Run(fmt.Sprintf("%s %s [%s]", route.method, route.path, ac.name), func(t *testing.T) {
				var body io.Reader
				if route.body != "" {
					body = strings.NewReader(route.body)
				}
				req := httptest.NewRequest(route.method, route.path, body)
				if route.contentType != "" {
					req.Header.Set("Content-Type", route.contentType)
				}
				if ac.sendCookie {
					req.AddCookie(&http.Cookie{Name: ac.cookieName, Value: ac.cookieVal})
				}

				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("expected 401 Unauthorized, got %d. Body: %s", rec.Code, rec.Body.String())
				}

				var errResp map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("expected JSON response, got error %v: %s", err, rec.Body.String())
				}
				if errResp["error"] != "Sign in" {
					t.Fatalf("expected error message 'Sign in', got %q", errResp["error"])
				}
			})
		}
	}
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(newMem(), dir, ""), dir
}

func signUp(t *testing.T, srv *Server, email string) string {
	t.Helper()
	body := strings.NewReader(`{"email":"` + email + `","password":"password123"}`)
	res := call(srv, http.MethodPost, "/api/register", "", body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("register %d: %s", res.Code, res.Body.String())
	}
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie.Value
		}
	}
	t.Fatal("register did not set a session")
	return ""
}

func getUserID(t *testing.T, srv *Server, cookie string) string {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/me", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("me status %d: %s", res.Code, res.Body.String())
	}
	var me account
	if err := json.Unmarshal(res.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	return me.ID
}

func createSheet(t *testing.T, srv *Server, cookie, title, address string) board.Job {
	t.Helper()
	body := strings.NewReader(`{"title":"` + title + `","address":"` + address + `"}`)
	res := call(srv, http.MethodPost, "/api/jobs", cookie, body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("create %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func uploadFileRaw(srv *Server, cookie, id, name string, data []byte, contentType string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		panic(err)
	}
	if _, err := part.Write(data); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return call(srv, http.MethodPost, "/api/jobs/"+id+"/file", cookie, &buf, writer.FormDataContentType())
}

func uploadFile(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) {
	t.Helper()
	res := uploadFileRaw(srv, cookie, id, name, data, contentType)
	if res.Code != http.StatusOK {
		t.Fatalf("upload %d: %s", res.Code, res.Body.String())
	}
}

func decodeJob(t *testing.T, res *httptest.ResponseRecorder) board.Job {
	t.Helper()
	var job board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func call(srv *Server, method, path, cookie string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	return res
}

type mem struct {
	mu       sync.Mutex
	users    map[string]board.User
	byEmail  map[string]string
	sessions map[string]memSession
	jobs     map[string]board.Job
}

type memSession struct {
	userID  string
	expires time.Time
}

func newMem() *mem {
	return &mem{
		users:    map[string]board.User{},
		byEmail:  map[string]string{},
		sessions: map[string]memSession{},
		jobs:     map[string]board.Job{},
	}
}

func (m *mem) CreateUser(_ context.Context, email, passwordHash string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return board.User{}, board.ErrDuplicate
	}
	user := board.User{ID: board.NewID(), Email: email, PasswordHash: passwordHash}
	m.users[user.ID] = user
	m.byEmail[email] = user.ID
	return user, nil
}

func (m *mem) UserByEmail(_ context.Context, email string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return m.users[id], nil
}

func (m *mem) CreateSession(_ context.Context, userID, token string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = memSession{userID: userID, expires: expires}
	return nil
}

func (m *mem) DeleteSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *mem) UserByToken(_ context.Context, token string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[token]
	if !ok || time.Now().After(session.expires) {
		return board.User{}, board.ErrNotFound
	}
	user, ok := m.users[session.userID]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return user, nil
}

func (m *mem) ListJobs(_ context.Context, userID string) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []board.Job
	for _, job := range m.jobs {
		if job.UserID == userID {
			jobs = append(jobs, cloneJob(job))
		}
	}
	if jobs == nil {
		jobs = []board.Job{}
	}
	return jobs, nil
}

func (m *mem) CreateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func (m *mem) Job(_ context.Context, id string) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return board.Job{}, board.ErrNotFound
	}
	return cloneJob(job), nil
}

func (m *mem) UpdateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[job.ID]; !ok {
		return board.ErrNotFound
	}
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func (m *mem) RunningJobs(_ context.Context) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []board.Job
	for _, job := range m.jobs {
		if job.Status == board.StatusRunning {
			jobs = append(jobs, cloneJob(job))
		}
	}
	if jobs == nil {
		jobs = []board.Job{}
	}
	return jobs, nil
}

func cloneJob(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}
