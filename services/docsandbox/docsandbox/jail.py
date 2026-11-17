"""The nsjail command line for one render (documents design §5 step 4).

Each file gets a fresh nsjail process: new mount, PID, IPC, UTS and network
namespaces (no interface at all, not even loopback), an empty tmpfs root
with only the system's read-only directories, the renderer, the job's input
file (read-only) and its output directory bound in; uid 65532; no
capabilities and no new privileges; 2 GB of address space, 60 s of CPU, 90 s
of wall time, 64 open files and 128 MB per written file; /proc not
mounted; a seccomp filter; an empty environment. Any limit kills it, and
the server reports ``unreadable``.

The docsandbox container holds no credential, no key and no network
(compose: network_mode none); the jail is the inner wall.
"""

from __future__ import annotations

import os

NSJAIL = "/usr/local/bin/nsjail"
PYTHON = "/usr/local/bin/python3"
RENDERER = "/opt/docsandbox/render.py"
# The uid and gid the renderer runs as: the documents service's (distroless
# nonroot), so the pages it writes belong to the service that reads them.
UID = 65532

# Address space (MB), CPU (s), wall time (s), file size (MB), open files.
MEMORY_MB = 2048
CPU_SECONDS = 60
WALL_SECONDS = 90
FILE_MB = 128
OPEN_FILES = 64

# The seccomp filter (design §17, PR 4): an allow-list, DEFAULT KILL. The
# list is what the renderer and the self-test were measured to call
# (strace over every synthetic sample in tests/samples.py: PDF, PNG, JPEG,
# TIFF, the refusals and the bombs), plus a small margin of calls that act
# only on the process itself (signals, time, its own ids and memory),
# in kafel's names (fstat is newfstat, uname newuname). Any
# other call kills the renderer (SIGSYS: unreadable, never a pass). An
# allow-list also closes the x32 ABI (numbers with bit 0x40000000) and
# 32-bit calls (int 0x80), whose numbers match nothing allowed.
#   - socket calls are EPERM (there is no interface anyway);
#   - clone only without a namespace flag (a thread, if a library makes
#     one); clone3, whose flags a filter can't read, is ENOSYS, so the C
#     library falls back to clone;
#   - nothing that mounts, unshares, traces, loads modules or keys, or
#     reaches io_uring, bpf or another process is allowed.
CLONE_NAMESPACE_FLAGS = 0x7E020080  # NEWNS|NEWCGROUP|NEWUTS|NEWIPC|NEWUSER|NEWPID|NEWNET|NEWTIME
ALLOWED = (
    # measured
    "access arch_prctl brk close epoll_create1 execve exit_group fcntl newfstat futex getdents64 getrandom gettid "
    "ioctl lseek mmap mprotect mremap munmap newfstatat openat pread64 prlimit64 read readlink readlinkat rename "
    "rseq rt_sigaction rt_sigprocmask set_robust_list set_tid_address newuname write "
    # margin: the process's own state only
    "rt_sigreturn sigaltstack exit madvise clock_gettime clock_getres gettimeofday nanosleep clock_nanosleep "
    "sched_yield sched_getaffinity getpid getppid getuid geteuid getgid getegid getcwd dup dup2 dup3 statx "
    "fstatfs writev readv pwrite64 ftruncate fsync fdatasync unlink"
).split()
SECCOMP = """
POLICY docsandbox {
  ALLOW {
    %s,
    clone(flags) { (flags & %#x) == 0 }
  },
  ERRNO(38) {
    clone3
  },
  ERRNO(1) {
    socket, socketpair, connect, bind, listen, accept, accept4
  }
}
USE docsandbox DEFAULT KILL
""" % (", ".join(ALLOWED), CLONE_NAMESPACE_FLAGS)


def argv(job_dir: str, program: list[str]) -> list[str]:
    """The nsjail argv that runs program with job_dir's in and out bound."""
    a = [
        NSJAIL,
        "--mode", "o",
        "--quiet",
        "--hostname", "docsandbox",
        "--cwd", "/tmp",
        # The container runs nsjail as root with CAP_SYS_ADMIN, so the jail
        # needs no user namespace: it drops to UID and every capability.
        "--disable_clone_newuser",
        "--user", str(UID),
        "--group", str(UID),
        "--rlimit_as", str(MEMORY_MB),
        "--rlimit_cpu", str(CPU_SECONDS),
        "--rlimit_fsize", str(FILE_MB),
        "--rlimit_nofile", str(OPEN_FILES),
        "--rlimit_core", "0",
        "--time_limit", str(WALL_SECONDS),
        "--max_cpus", "1",
        "--disable_proc",
        "--iface_no_lo",
        "--bindmount_ro", "/usr",
        "--symlink", "/usr/lib:/lib",
        "--bindmount_ro", "/opt/docsandbox/render.py:" + RENDERER,
        # The loader's cache: libpython lives in /usr/local/lib.
        "--bindmount_ro", "/etc/ld.so.cache",
        "--bindmount_ro", "/dev/null",
        "--bindmount_ro", "/dev/urandom",
        "--tmpfsmount", "/tmp",
        "--bindmount_ro", os.path.join(job_dir, "in") + ":/job/in",
        "--bindmount", os.path.join(job_dir, "out") + ":/job/out",
        "--env", "HOME=/tmp",
        "--env", "PYTHONDONTWRITEBYTECODE=1",
        "--seccomp_string", SECCOMP,
    ]
    if os.path.isdir("/usr/lib64"):
        a += ["--symlink", "/usr/lib64:/lib64"]
    return [*a, "--", *program]


def render_program(mime: str, max_pages: int) -> list[str]:
    """The renderer's command line inside the jail."""
    return [PYTHON, "-I", RENDERER, "/job/in", "/job/out", mime, str(max_pages)]
