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

# The seccomp filter (design §17, PR 4). Not yet an allow-list (DEFAULT
# KILL needs the syscalls of every pdfium and Pillow path traced from real
# renders; TODO.md): a deny-list that closes the ways out of the jail's
# namespaces and the kernel's large attack surfaces:
#   - every socket call is EPERM (there is no interface anyway);
#   - clone with any namespace flag, unshare and setns are killed, and
#     clone3 (whose flags a filter can't read) is ENOSYS, so the C library
#     falls back to clone: no new user, mount, PID or network namespace;
#   - io_uring, the new mount API (fsopen, fsconfig, fsmount, fspick,
#     open_tree, move_mount, mount_setattr), pidfd_getfd, ptrace, process_vm_*,
#     bpf, perf_event_open, userfaultfd, keyrings, module and kexec calls,
#     handle-based opens and the clock and swap calls are killed.
# The renderer runs as uid 65532 with no capability and no_new_privs, so
# what stays allowed acts only on its own tmpfs root and its own files.
CLONE_NAMESPACE_FLAGS = 0x7E020080  # NEWNS|NEWCGROUP|NEWUTS|NEWIPC|NEWUSER|NEWPID|NEWNET|NEWTIME
SECCOMP = """
POLICY docsandbox {
  KILL {
    clone(flags) { (flags & %#x) != 0 },
    ptrace, process_vm_readv, process_vm_writev, mount, umount, pivot_root, chroot,
    unshare, setns, keyctl, add_key, request_key, bpf, perf_event_open, userfaultfd,
    kexec_load, kexec_file_load, init_module, finit_module, delete_module,
    open_by_handle_at, name_to_handle_at, swapon, swapoff, reboot, iopl, ioperm,
    acct, quotactl, settimeofday, clock_settime, adjtimex, syslog, vhangup,
    fanotify_init, lookup_dcookie, move_pages, migrate_pages,
    io_uring_setup, io_uring_enter, io_uring_register,
    fsopen, fsconfig, fsmount, fspick, open_tree, move_mount, mount_setattr, pidfd_getfd
  },
  ERRNO(38) {
    clone3
  },
  ERRNO(1) {
    socket, socketpair, connect, bind, listen, accept, accept4
  }
}
USE docsandbox DEFAULT ALLOW
""" % CLONE_NAMESPACE_FLAGS


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
