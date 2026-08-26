#!/usr/bin/env python3
import gzip
import os
import posixpath
import shutil
import stat
import sys
import tarfile


class LimitedReader:
    def __init__(self, source, limit):
        self.source = source
        self.limit = limit
        self.count = 0

    def read(self, size=-1):
        remaining = self.limit - self.count
        if remaining < 0:
            raise ValueError("source archive exceeds decompressed size limit")
        if size < 0 or size > remaining + 1:
            size = remaining + 1
        value = self.source.read(size)
        self.count += len(value)
        if self.count > self.limit:
            raise ValueError("source archive exceeds decompressed size limit")
        return value


def archive_name(raw):
    if not raw or "\0" in raw or "\\" in raw or raw.startswith("/"):
        raise ValueError(f"unsafe archive path: {raw!r}")
    parts = raw.rstrip("/").split("/")
    if any(part in ("", ".", "..") for part in parts):
        raise ValueError(f"unsafe archive path: {raw!r}")
    name = "/".join(parts)
    if name not in ("source.bundle", "deleted", "worktree") and not name.startswith("worktree/"):
        raise ValueError(f"unexpected archive path: {raw!r}")
    return name


def safe_link(name, target):
    if not target or "\0" in target or "\\" in target or posixpath.isabs(target):
        return False
    resolved = posixpath.normpath(posixpath.join(posixpath.dirname(name), target))
    return resolved == "worktree" or resolved.startswith("worktree/")


def ensure_directory(root, parts):
    current = root
    for part in parts:
        current = os.path.join(current, part)
        try:
            mode = os.lstat(current).st_mode
        except FileNotFoundError:
            os.mkdir(current, 0o755)
            continue
        if stat.S_ISLNK(mode) or not stat.S_ISDIR(mode):
            raise ValueError(f"archive path crosses non-directory: {part!r}")
    return current


def extract(archive_path, destination, limit):
    if os.path.lexists(destination):
        raise ValueError("extraction destination already exists")
    os.mkdir(destination, 0o700)
    seen = set()
    required = set()
    payload_bytes = 0
    try:
        with open(archive_path, "rb") as compressed:
            with gzip.GzipFile(fileobj=compressed) as uncompressed:
                limited = LimitedReader(uncompressed, limit)
                with tarfile.open(fileobj=limited, mode="r|") as archive:
                    for member in archive:
                        name = archive_name(member.name)
                        if name in seen:
                            raise ValueError(f"duplicate archive path: {name!r}")
                        seen.add(name)
                        parts = name.split("/")
                        parent = ensure_directory(destination, parts[:-1])
                        target = os.path.join(parent, parts[-1])
                        if member.type in (tarfile.REGTYPE, tarfile.AREGTYPE):
                            if name == "worktree":
                                raise ValueError("worktree must be a directory")
                            if member.size < 0 or payload_bytes + member.size > limit:
                                raise ValueError("source archive payload exceeds size limit")
                            if os.path.lexists(target):
                                raise ValueError(f"archive path conflicts with existing path: {name!r}")
                            source = archive.extractfile(member)
                            if source is None:
                                raise ValueError(f"cannot read archive member: {name!r}")
                            mode = 0o755 if member.mode & 0o111 else 0o644
                            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
                            with os.fdopen(descriptor, "wb") as output:
                                remaining = member.size
                                while remaining:
                                    chunk = source.read(min(1024 * 1024, remaining))
                                    if not chunk:
                                        raise ValueError(f"truncated archive member: {name!r}")
                                    output.write(chunk)
                                    remaining -= len(chunk)
                            payload_bytes += member.size
                            if name in ("source.bundle", "deleted"):
                                required.add(name)
                        elif member.isdir():
                            if name in ("source.bundle", "deleted"):
                                raise ValueError(f"archive member must be a file: {name!r}")
                            if os.path.lexists(target):
                                mode = os.lstat(target).st_mode
                                if stat.S_ISLNK(mode) or not stat.S_ISDIR(mode):
                                    raise ValueError(f"archive directory conflicts with path: {name!r}")
                            else:
                                os.mkdir(target, 0o755)
                            if name == "worktree":
                                required.add(name)
                        elif member.issym():
                            if not name.startswith("worktree/") or not safe_link(name, member.linkname):
                                raise ValueError(f"unsafe archive symlink: {name!r}")
                            if os.path.lexists(target):
                                raise ValueError(f"archive symlink conflicts with path: {name!r}")
                            os.symlink(member.linkname, target)
                        else:
                            raise ValueError(f"unsupported archive member type: {name!r}")
        if required != {"source.bundle", "deleted", "worktree"}:
            raise ValueError("source archive is missing required entries")
    except Exception:
        shutil.rmtree(destination, ignore_errors=True)
        raise


def relative_path(raw):
    if not raw or "\0" in raw or "\\" in raw or raw.startswith("/"):
        raise ValueError(f"unsafe worktree path: {raw!r}")
    parts = raw.split("/")
    if any(part in ("", ".", "..", ".git") for part in parts):
        raise ValueError(f"unsafe worktree path: {raw!r}")
    return parts


def target_parent(root, parts, create):
    current = root
    for part in parts:
        current = os.path.join(current, part)
        try:
            mode = os.lstat(current).st_mode
        except FileNotFoundError:
            if not create:
                return None
            os.mkdir(current, 0o755)
            continue
        if stat.S_ISLNK(mode) or not stat.S_ISDIR(mode):
            raise ValueError(f"worktree path crosses non-directory: {part!r}")
    return current


def remove_path(path):
    try:
        mode = os.lstat(path).st_mode
    except FileNotFoundError:
        return
    if stat.S_ISDIR(mode) and not stat.S_ISLNK(mode):
        shutil.rmtree(path)
    else:
        os.unlink(path)


def apply_entry(source, repository, relative):
    parts = relative_path(relative)
    parent = target_parent(repository, parts[:-1], True)
    target = os.path.join(parent, parts[-1])
    mode = os.lstat(source).st_mode
    if stat.S_ISDIR(mode) and not stat.S_ISLNK(mode):
        if os.path.lexists(target):
            target_mode = os.lstat(target).st_mode
            if stat.S_ISLNK(target_mode) or not stat.S_ISDIR(target_mode):
                remove_path(target)
                os.mkdir(target, 0o755)
        else:
            os.mkdir(target, 0o755)
        for entry in os.scandir(source):
            apply_entry(entry.path, repository, relative + "/" + entry.name)
        return
    remove_path(target)
    if stat.S_ISLNK(mode):
        link = os.readlink(source)
        if not safe_link("worktree/" + relative, link):
            raise ValueError(f"unsafe overlay symlink: {relative!r}")
        os.symlink(link, target)
        return
    if not stat.S_ISREG(mode):
        raise ValueError(f"unsupported overlay entry: {relative!r}")
    descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o755 if mode & 0o111 else 0o644)
    with open(source, "rb") as value, os.fdopen(descriptor, "wb") as output:
        shutil.copyfileobj(value, output, 1024 * 1024)


def apply(overlay, deleted_path, repository):
    if not os.path.isdir(overlay) or os.path.islink(overlay):
        raise ValueError("overlay is not a directory")
    if not os.path.isdir(os.path.join(repository, ".git")) or os.path.islink(repository):
        raise ValueError("repository is not a safe Git checkout")
    if os.path.getsize(deleted_path) > 64 * 1024 * 1024:
        raise ValueError("deleted path list is too large")
    with open(deleted_path, "rb") as source:
        deleted = source.read()
    if deleted and not deleted.endswith(b"\0"):
        raise ValueError("deleted path list is not NUL terminated")
    for raw in deleted.split(b"\0"):
        if not raw:
            continue
        parts = relative_path(os.fsdecode(raw))
        parent = target_parent(repository, parts[:-1], False)
        if parent is not None:
            remove_path(os.path.join(parent, parts[-1]))
    for entry in os.scandir(overlay):
        apply_entry(entry.path, repository, entry.name)


def main():
    if len(sys.argv) == 5 and sys.argv[1] == "extract":
        extract(sys.argv[2], sys.argv[3], int(sys.argv[4]))
        return
    if len(sys.argv) == 5 and sys.argv[1] == "apply":
        apply(sys.argv[2], sys.argv[3], sys.argv[4])
        return
    raise SystemExit("usage: source_extract.py extract ARCHIVE DEST LIMIT | apply OVERLAY DELETED REPOSITORY")


if __name__ == "__main__":
    main()
