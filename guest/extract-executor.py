#!/usr/bin/env python3
import hashlib
import json
import os
import shutil
import sys
import tarfile
from pathlib import Path, PurePosixPath


def fail(message: str) -> None:
    raise SystemExit(message)


def digest_path(directory: Path, digest: str) -> Path:
    algorithm, separator, value = digest.partition(":")
    if algorithm != "sha256" or separator != ":" or len(value) != 64:
        fail(f"unsupported OCI digest: {digest}")
    try:
        int(value, 16)
    except ValueError:
        fail(f"invalid OCI digest: {digest}")
    return directory / value


def verify_digest(path: Path, digest: str) -> None:
    expected = digest.removeprefix("sha256:")
    hasher = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            hasher.update(chunk)
    actual = hasher.hexdigest()
    if actual != expected:
        fail(f"digest mismatch for {path.name}: got sha256:{actual}, want {digest}")


def normalized_parts(name: str) -> tuple[str, ...]:
    path = PurePosixPath(name)
    parts = tuple(part for part in path.parts if part not in ("", "."))
    if path.is_absolute() or ".." in parts:
        fail(f"unsafe OCI layer path: {name!r}")
    return parts


def selected(parts: tuple[str, ...]) -> bool:
    if not parts:
        return False
    if parts[-1].startswith(".wh.") and parts[-1] != ".wh..wh..opq":
        target = parts[-1].removeprefix(".wh.")
        if not target:
            return False
        parts = (*parts[:-1], target)
    elif parts[-1] == ".wh..wh..opq":
        parts = parts[:-1]
        if not parts:
            return False
    bun = ("usr", "local", "bin", "bun")
    return parts[0] == "app" or parts == bun[: len(parts)]


def safe_destination(root: Path, parts: tuple[str, ...]) -> Path:
    candidate = root.joinpath(*parts)
    if not candidate.parent.resolve(strict=False).is_relative_to(root.resolve()):
        fail(f"OCI layer path escapes destination: {'/'.join(parts)}")
    return candidate


def remove_path(path: Path) -> None:
    try:
        path.lstat()
    except FileNotFoundError:
        return
    if path.is_dir() and not path.is_symlink():
        shutil.rmtree(path)
    else:
        path.unlink()


def apply_whiteout(root: Path, parts: tuple[str, ...]) -> None:
    name = parts[-1]
    parent = safe_destination(root, parts[:-1]) if len(parts) > 1 else root
    if name == ".wh..wh..opq":
        if parent.exists():
            if parent.is_symlink() or not parent.resolve().is_relative_to(root.resolve()):
                fail(f"unsafe opaque whiteout parent: {'/'.join(parts[:-1])}")
            for child in parent.iterdir():
                remove_path(child)
        return
    target_name = name.removeprefix(".wh.")
    if not target_name or "/" in target_name:
        fail(f"invalid OCI whiteout: {'/'.join(parts)}")
    remove_path(safe_destination(root, (*parts[:-1], target_name)))


def extract_layer(layer_path: Path, destination: Path) -> None:
    with tarfile.open(layer_path, "r:*") as archive:
        members = archive.getmembers()
        parsed = [(member, normalized_parts(member.name)) for member in members]

        for _, parts in parsed:
            if selected(parts) and parts[-1].startswith(".wh."):
                apply_whiteout(destination, parts)

        for member, parts in parsed:
            if not selected(parts) or parts[-1].startswith(".wh."):
                continue
            if member.isdev() or member.isfifo():
                fail(f"unsupported special file in OCI payload: {member.name}")
            try:
                archive.extract(member, destination, filter="data")
            except (tarfile.FilterError, OSError) as error:
                fail(f"unsafe OCI member {member.name!r}: {error}")


def main() -> None:
    if len(sys.argv) != 3:
        fail("usage: extract-executor.py SKOPEO_DIR DESTINATION")

    source = Path(sys.argv[1]).resolve()
    destination = Path(sys.argv[2]).resolve()
    manifest_path = source / "manifest.json"
    if not manifest_path.is_file():
        fail("skopeo directory does not contain manifest.json")

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    config_digest = manifest.get("config", {}).get("digest", "")
    config_path = digest_path(source, config_digest)
    verify_digest(config_path, config_digest)
    config = json.loads(config_path.read_text(encoding="utf-8"))
    if config.get("os") != "linux" or config.get("architecture") != "arm64":
        fail(
            "Executor image must resolve to linux/arm64, got "
            f"{config.get('os')}/{config.get('architecture')}"
        )

    destination.mkdir(parents=True, exist_ok=False)
    for descriptor in manifest.get("layers", []):
        digest = descriptor.get("digest", "")
        layer_path = digest_path(source, digest)
        verify_digest(layer_path, digest)
        extract_layer(layer_path, destination)

    app = destination / "app" / "apps" / "host-selfhost" / "src" / "serve.ts"
    bun = destination / "usr" / "local" / "bin" / "bun"
    if not app.is_file() or not bun.is_file():
        fail("Executor OCI payload is missing the self-host app or bundled Bun runtime")
    bun.chmod(0o755)

    manifest_digest = hashlib.sha256(manifest_path.read_bytes()).hexdigest()
    print(f"sha256:{manifest_digest}")


if __name__ == "__main__":
    main()
