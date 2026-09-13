# Isolated PostgreSQL test runtime

User-approved on 2026-09-14. The runtime is **outside Git and outside the project**.
No Homebrew install, sudo, launch agent, Docker daemon, system PATH edit or production
connection is involved. Ordinary recovery tests never download or start PostgreSQL.

## Source and build

`source.json` pins PostgreSQL 18.6 from the [official release directory](https://www.postgresql.org/ftp/source/v18.6/)
and its [official SHA-256](https://ftp.postgresql.org/pub/source/v18.6/postgresql-18.6.tar.gz.sha256).
The archive was downloaded over HTTPS, matched the pinned hash and size, and was
checked for relative `postgresql-18.6/` regular-file/directory tar entries only
before extraction into a new private directory. No binary release was trusted by version string alone.

The local build used Apple clang 14.0.3, macOS arm64, system make/flex/bison, and an
explicit `--prefix=<new-private-directory>/install`. With no local ICU development
package, `--without-icu --without-readline` keeps the test build self-contained.
These are [documented configure options](https://www.postgresql.org/docs/18/install-make.html),
not claims of production feature or locale parity. `citext` was built from the
same source using `make -C contrib/citext install`; no unrelated extensions were installed.

To reproduce, use a new project-external `mktemp -d` directory. Download the exact
archive, compare `source.json`'s SHA-256 **before extracting or executing it**, reject
absolute/traversing/link/device tar entries, then from the extracted source run:

```sh
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin LC_ALL=C LANG=C CC=/usr/bin/clang \
  ./configure --prefix=/ABSOLUTE/NEW/PRIVATE/DIRECTORY/install --without-icu --without-readline
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin LC_ALL=C LANG=C make -s -j4
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin LC_ALL=C LANG=C make -s install
env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin LC_ALL=C LANG=C make -s -C contrib/citext install
```

Replace the prefix with your **new external directory**, not a system or repository
directory. The compiler path above is the tested macOS toolchain; other platforms
need their own validated build. Do not execute a build whose checksum differs.

## Run the database gate

From the repository root, explicitly select your isolated runtime:

```sh
PORTUNEX_TEST_PG_BIN=/ABSOLUTE/ISOLATED/install/bin make -C recovery postgres-integration
```

The current helper requires exact server version 18.6, supported on macOS/Linux.
Missing or wrong binaries and any inherited `PG*`, `DATABASE_URL` or `DATABASE_DSN`
variable cause a **failure**, not a skip or fallback. Remove routing variables only
from the test command's environment; do not modify the user's global environment.
Private socket temp paths must use the documented narrow ASCII path set (letters,
digits, slash, dot, hyphen, underscore); unsupported TMPDIR paths fail before startup.

Each test makes a new cluster and synthetic database through a private Unix socket,
with TCP disabled and no real accounts or credentials. Successful normal shutdown
removes only that test's newly created directory. Abnormal bootstrap/shutdown reports
failure and retains the directory, because a reaped postmaster does not prove that
every PostgreSQL child has exited. Such a failure requires inspecting the **owned**
instance before cleanup; never use a broad `pkill postgres` or recursive deletion.

The gate tests the recovery schema and storage primitives, not legacy HTTP login,
token lifetime or full original-system compatibility. C-locale ASCII citext behavior
does not verify production locale/Unicode case behavior. Runtime provenance and
executed checks are recorded in the dated recovery progress document.
