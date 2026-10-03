// Package store persists daemon service registrations and file access policy in
// registry.json. Blob names are lowercase SHA-256 digests; arbitrary paths are
// never accepted as keys. New blobs deny every peer until explicitly granted.
//
// # Filesystem contract
//
// Use a dedicated local directory owned by the daemon's OS account. Open creates
// directories with mode 0700 and files with mode 0600. On Windows, the operator
// must also provide an account-private NTFS ACL (Unix mode bits do not establish
// one). Directory ancestors and the daemon's OS account are trusted. Root/blob
// directory symlinks, registry/blob symlinks, and special files are rejected;
// portable Go checks do not prevent a malicious same-account process from
// replacing files or directories after a check. Do not use a shared directory,
// a network filesystem, or a filesystem without same-directory atomic rename.
// Immutable means the daemon never rewrites committed blob content; it does not
// protect files from the owner or a privileged process editing them externally.
// OpenFile hashes a descriptor using bounded memory before returning it, then
// serves that same descriptor. External writers violate the private-directory
// boundary and could change a file after verification.
//
// The .daemon-lock directory is acquired by exclusive mkdir on all platforms.
// It is removed only by the daemon's normal Close. After a crash, the operator
// must confirm that no daemon is using the directory, then remove only owner
// and the empty .daemon-lock directory. The PID in owner is diagnostic, never
// authority to steal a lock (PID reuse and other hosts make that unsafe).
// Registry and blob staging files left by a crash are cleaned only after the
// lock is acquired and the registry validates. A digest-named blob unreferenced
// by the valid registry is also cleaned as an interrupted pre-registry commit.
// A missing registry with committed-looking blobs fails closed for recovery.
//
// # Durability
//
// Each blob is flushed, renamed, and its directory flushed before registry
// replacement. Registry updates are flushed to a temporary file then committed
// by same-directory rename; in-memory state changes only after that succeeds.
// On POSIX filesystems the directory is flushed after registry replacement on a
// best-effort basis. Once replacement succeeds, a later directory-flush failure
// cannot safely be reported as a rollback. On Windows portable Go cannot flush
// directory handles, so sudden-power-loss durability depends on the filesystem;
// process-crash atomicity still requires the filesystem's rename semantics.
// JSON is bounded, versioned, and rejects unknown and duplicate members. No
// authentication tokens, keys, or passwords belong in this registry. Service
// URLs reject credentials, query strings, and fragments; do not encode secrets
// in URL path segments either.
package store
