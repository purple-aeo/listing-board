# Notes

Cursor suggested using `r.ParseMultipartForm(32 << 20)` and determining the brochure file type from the `Content-Type` header and file extension (such as `.pdf` and `.txt`).

I rejected this because headers and file extensions can be trivially spoofed, and multipart form parsing can spill large uploads to temporary files on disk. The specification strictly requires determining the file type solely from content bytes (%PDF- prefix or valid UTF-8 without NUL bytes) and rejecting files larger than 1 MiB without leaving temporary files on disk.

To check this, I tested uploading non-PDF binary files disguised with a `.pdf` extension, valid text with mismatched headers, and oversized payloads exceeding 1 MiB, verifying that the server rejects invalid uploads and never leaves orphaned files on disk.
