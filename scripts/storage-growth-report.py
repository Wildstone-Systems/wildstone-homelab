#!/usr/bin/env python3
"""Emit a privacy-safe, read-only host storage growth sample as JSON."""

from __future__ import annotations

import datetime as dt
import json
import os
from pathlib import Path
import shutil
import stat as statmode
import subprocess
import sys
from typing import Any


RESTIC_IMAGE = (
    "restic/restic@sha256:9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481"
)
DOCKER = ["docker", "--host", "unix:///var/run/docker.sock"]
CONTAINERD = [
    "ctr",
    "--address",
    "/run/k3s/containerd/containerd.sock",
    "--namespace",
    "k8s.io",
]
CACHE_PATHS = {
    "apt-archives": "/var/cache/apt/archives",
    "wildstone-build": "/var/cache/wildstone/build",
    "wildstone-go-build": "/var/cache/wildstone/go-build",
    "wildstone-tools": "/var/cache/wildstone/tools",
}


class Collector:
    def __init__(self) -> None:
        self.root = Path(os.environ.get("STORAGE_REPORT_ROOT", "/"))
        self.errors: list[str] = []
        self.retention_days = self.positive_int("RETENTION_DAYS", 30)
        self.min_free_bytes = self.nonnegative_int("MIN_FREE_BYTES", 16 * 1024**3)
        self.now = self.timestamp()

    def nonnegative_int(self, name: str, default: int) -> int:
        value = os.environ.get(name, str(default))
        if not value.isdecimal() or len(value) > 18:
            raise ValueError(f"{name} must be a decimal integer of at most 18 digits")
        return int(value, 10)

    def positive_int(self, name: str, default: int) -> int:
        value = self.nonnegative_int(name, default)
        if value == 0:
            raise ValueError(f"{name} must be greater than zero")
        return value

    def timestamp(self) -> dt.datetime:
        configured = os.environ.get("REPORT_TIMESTAMP")
        if configured:
            parsed = dt.datetime.fromisoformat(configured.replace("Z", "+00:00"))
            if parsed.tzinfo is None:
                raise ValueError("REPORT_TIMESTAMP must include a timezone")
            return parsed.astimezone(dt.timezone.utc)
        return dt.datetime.now(dt.timezone.utc)

    def path(self, absolute: str) -> Path:
        return self.root / absolute.lstrip("/")

    def tree_scan(
        self, absolute: str, error_code: str, *, include_files: bool = False
    ) -> tuple[int, str, list[tuple[int, float]]]:
        path = self.path(absolute)
        try:
            root_stat = path.lstat()
        except FileNotFoundError:
            return 0, "not_present", []
        except OSError:
            self.errors.append(error_code)
            return 0, "incomplete", []
        total = 0
        files: list[tuple[int, float]] = []
        try:
            root_device = root_stat.st_dev
            total += root_stat.st_blocks * 512
            if not statmode.S_ISDIR(root_stat.st_mode):
                raise NotADirectoryError
            pending = [path]
            while pending:
                current = pending.pop()
                with os.scandir(current) as entries:
                    for entry in entries:
                        entry_stat = entry.stat(follow_symlinks=False)
                        total += entry_stat.st_blocks * 512
                        if statmode.S_ISDIR(entry_stat.st_mode):
                            if entry_stat.st_dev == root_device:
                                pending.append(Path(entry.path))
                        elif include_files and statmode.S_ISREG(entry_stat.st_mode):
                            files.append((entry_stat.st_blocks * 512, entry_stat.st_mtime))
        except (OSError, PermissionError):
            self.errors.append(error_code)
            return total, "incomplete", []
        return total, "complete", files

    def tree_bytes(self, absolute: str, error_code: str) -> tuple[int, str]:
        total, status, _ = self.tree_scan(absolute, error_code)
        return total, status

    def command(self, argv: list[str], error_code: str) -> str | None:
        try:
            result = subprocess.run(
                argv,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=30,
            )
        except (OSError, subprocess.TimeoutExpired):
            self.errors.append(error_code)
            return None
        if result.returncode != 0:
            self.errors.append(error_code)
            return None
        return result.stdout

    def optional_command(self, argv: list[str]) -> str | None:
        """Run a lookup where a nonzero result means the object is absent."""
        try:
            result = subprocess.run(
                argv,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=30,
            )
        except (OSError, subprocess.TimeoutExpired):
            return None
        return result.stdout if result.returncode == 0 else None

    @staticmethod
    def nonempty_lines(output: str) -> list[str]:
        return [line.strip() for line in output.splitlines() if line.strip()]

    def protected_images(self) -> tuple[list[str], bool]:
        refs = {RESTIC_IMAGE}
        configured = os.environ.get(
            "PROTECTED_IMAGE_FILE", "/etc/wildstone/storage-growth.protected-images"
        )
        path = self.path(configured)
        if not path.exists():
            return sorted(refs), True
        try:
            for line in path.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if line and not line.startswith("#"):
                    refs.add(line)
        except (OSError, UnicodeError):
            self.errors.append("protected_image_inventory_unreadable")
            return sorted(refs), False
        return sorted(refs), True

    def docker(self) -> dict[str, Any]:
        image_metadata_bytes, image_status = self.tree_bytes(
            "/var/lib/docker/image", "docker_image_storage_unreadable"
        )
        snapshot_bytes, snapshot_status = self.tree_bytes(
            "/var/lib/docker/overlay2", "docker_snapshot_storage_unreadable"
        )
        volume_bytes, volume_status = self.tree_bytes(
            "/var/lib/docker/volumes", "docker_volume_storage_unreadable"
        )
        storage_present = any(
            status != "not_present"
            for status in (image_status, snapshot_status, volume_status)
        )
        socket_present = self.path("/var/run/docker.sock").exists()
        if not storage_present and not socket_present:
            return {
                "presence": "not_present",
                "collectionStatus": "not_present",
                "imageBytes": 0,
                "imageMetadataBytes": 0,
                "imageStorageStatus": "not_present",
                "snapshotBytes": 0,
                "snapshotStorageStatus": "not_present",
                "volumeBytes": 0,
                "volumeStorageStatus": "not_present",
                "imageCount": None,
                "referencedImageCount": None,
                "unreferencedImageCount": None,
            }
        if shutil.which("docker") is None:
            self.errors.append("docker_binary_missing_with_runtime")
            return {
                "presence": "present",
                "collectionStatus": "incomplete",
                "imageBytes": None,
                "imageMetadataBytes": image_metadata_bytes,
                "imageStorageStatus": image_status,
                "snapshotBytes": snapshot_bytes,
                "snapshotStorageStatus": snapshot_status,
                "volumeBytes": volume_bytes,
                "volumeStorageStatus": volume_status,
                "imageCount": None,
                "referencedImageCount": None,
                "unreferencedImageCount": None,
            }

        images_output = self.command(
            [*DOCKER, "image", "ls", "--no-trunc", "--format", "{{.ID}}"],
            "docker_image_inventory_failed",
        )
        containers_output = self.command(
            [*DOCKER, "ps", "-aq", "--no-trunc"], "docker_container_inventory_failed"
        )
        protected_refs, protected_ok = self.protected_images()
        references: set[str] = set()
        references_ok = containers_output is not None and protected_ok
        if containers_output is not None:
            for container_id in self.nonempty_lines(containers_output):
                image_id = self.command(
                    [*DOCKER, "container", "inspect", "--format", "{{.Image}}", container_id],
                    "docker_container_reference_failed",
                )
                if image_id is None or len(self.nonempty_lines(image_id)) != 1:
                    references_ok = False
                else:
                    references.add(self.nonempty_lines(image_id)[0])
        for image_ref in protected_refs:
            image_id = self.optional_command(
                [*DOCKER, "image", "inspect", "--format", "{{.Id}}", image_ref],
            )
            if image_id is None:
                continue
            elif len(self.nonempty_lines(image_id)) == 1:
                references.add(self.nonempty_lines(image_id)[0])
            else:
                references_ok = False

        images = set(self.nonempty_lines(images_output)) if images_output is not None else set()
        if references - images:
            self.errors.append("docker_reference_unknown")
            references_ok = False
        image_bytes = 0
        image_sizes_ok = images_output is not None
        for image_id in images:
            size_output = self.command(
                [*DOCKER, "image", "inspect", "--format", "{{.Size}}", image_id],
                "docker_image_size_failed",
            )
            size_lines = self.nonempty_lines(size_output) if size_output is not None else []
            if len(size_lines) != 1 or not size_lines[0].isdecimal() or len(size_lines[0]) > 18:
                if size_output is not None:
                    self.errors.append("docker_image_size_malformed")
                image_sizes_ok = False
            else:
                image_bytes += int(size_lines[0], 10)
        statuses = (image_status, snapshot_status, volume_status)
        complete = (
            images_output is not None
            and image_sizes_ok
            and references_ok
            and all(status != "incomplete" for status in statuses)
        )
        referenced_count = len(images & references) if complete else None
        return {
            "presence": "present",
            "collectionStatus": "complete" if complete else "incomplete",
            "imageBytes": image_bytes,
            "imageMetadataBytes": image_metadata_bytes,
            "imageStorageStatus": image_status,
            "snapshotBytes": snapshot_bytes,
            "snapshotStorageStatus": snapshot_status,
            "volumeBytes": volume_bytes,
            "volumeStorageStatus": volume_status,
            "imageCount": len(images) if images_output is not None else None,
            "referencedImageCount": referenced_count,
            "unreferencedImageCount": len(images) - referenced_count if complete else None,
        }

    def containerd(self) -> dict[str, Any]:
        image_bytes, image_status = self.tree_bytes(
            "/var/lib/rancher/k3s/agent/containerd/io.containerd.content.v1.content",
            "containerd_image_storage_unreadable",
        )
        snapshot_bytes, snapshot_status = self.tree_bytes(
            "/var/lib/rancher/k3s/agent/containerd/io.containerd.snapshotter.v1.overlayfs",
            "containerd_snapshot_storage_unreadable",
        )
        storage_present = image_status != "not_present" or snapshot_status != "not_present"
        socket_present = self.path("/run/k3s/containerd/containerd.sock").exists()
        if not storage_present and not socket_present:
            return {
                "presence": "not_present",
                "collectionStatus": "not_present",
                "imageBytes": 0,
                "imageStorageStatus": "not_present",
                "snapshotBytes": 0,
                "snapshotStorageStatus": "not_present",
                "imageCount": None,
                "referencedImageCount": None,
                "unreferencedImageCount": None,
            }
        if shutil.which("ctr") is None:
            self.errors.append("containerd_binary_missing_with_runtime")
            return {
                "presence": "present",
                "collectionStatus": "incomplete",
                "imageBytes": image_bytes,
                "imageStorageStatus": image_status,
                "snapshotBytes": snapshot_bytes,
                "snapshotStorageStatus": snapshot_status,
                "imageCount": None,
                "referencedImageCount": None,
                "unreferencedImageCount": None,
            }

        images_output = self.command(
            [*CONTAINERD, "images", "list", "-q"],
            "containerd_image_inventory_failed",
        )
        containers_output = self.command(
            [*CONTAINERD, "containers", "list", "-q"],
            "containerd_container_inventory_failed",
        )
        protected_refs, protected_ok = self.protected_images()
        references: set[str] = set()
        references_ok = containers_output is not None and protected_ok
        if containers_output is not None:
            for container_id in self.nonempty_lines(containers_output):
                info = self.command(
                    [*CONTAINERD, "containers", "info", container_id],
                    "containerd_container_reference_failed",
                )
                if info is None:
                    references_ok = False
                    continue
                try:
                    image_ref = json.loads(info).get("Image")
                except json.JSONDecodeError:
                    image_ref = None
                if not isinstance(image_ref, str) or not image_ref:
                    self.errors.append("containerd_container_reference_malformed")
                    references_ok = False
                else:
                    references.add(image_ref)

        images = set(self.nonempty_lines(images_output)) if images_output is not None else set()
        if references - images:
            self.errors.append("containerd_reference_unknown")
            references_ok = False
        references.update(image_ref for image_ref in protected_refs if image_ref in images)
        complete = (
            images_output is not None
            and references_ok
            and image_status != "incomplete"
            and snapshot_status != "incomplete"
        )
        referenced_count = len(images & references) if complete else None
        return {
            "presence": "present",
            "collectionStatus": "complete" if complete else "incomplete",
            "imageBytes": image_bytes,
            "imageStorageStatus": image_status,
            "snapshotBytes": snapshot_bytes,
            "snapshotStorageStatus": snapshot_status,
            "imageCount": len(images) if images_output is not None else None,
            "referencedImageCount": referenced_count,
            "unreferencedImageCount": len(images) - referenced_count if complete else None,
        }

    def caches(self) -> tuple[dict[str, int], dict[str, str], list[dict[str, Any]]]:
        totals = {"buildCacheBytes": 0, "toolCacheBytes": 0}
        statuses: dict[str, str] = {}
        candidates: list[dict[str, Any]] = []
        cutoff = self.now.timestamp() - self.retention_days * 86400
        for cache_id, absolute in CACHE_PATHS.items():
            size, status, files = self.tree_scan(
                absolute, f"cache_{cache_id}_unreadable", include_files=True
            )
            key = "buildCacheBytes" if cache_id in {"wildstone-build", "wildstone-go-build"} else "toolCacheBytes"
            totals[key] += size
            statuses[cache_id] = status
            if status == "not_present":
                continue
            old_bytes = 0
            oldest = self.now.timestamp()
            if status == "incomplete":
                continue
            for allocated_bytes, modified_at in files:
                if modified_at < cutoff:
                    old_bytes += allocated_bytes
                    oldest = min(oldest, modified_at)
            if old_bytes:
                candidates.append(
                    {
                        "id": cache_id,
                        "bytes": old_bytes,
                        "oldestAgeDays": int((self.now.timestamp() - oldest) // 86400),
                        "retentionDays": self.retention_days,
                        "reason": "files_older_than_retention",
                    }
                )
        return totals, statuses, candidates

    def collect(self) -> dict[str, Any]:
        try:
            filesystem = os.statvfs(self.root)
            free_bytes = filesystem.f_bavail * filesystem.f_frsize
        except OSError:
            self.errors.append("filesystem_usage_failed")
            free_bytes = None

        docker = self.docker()
        containerd = self.containerd()
        cache_totals, cache_statuses, candidates = self.caches()
        log_bytes, log_status = self.tree_bytes("/var/log", "log_storage_unreadable")
        result = {
            "schemaVersion": 1,
            "collectedAt": self.now.isoformat().replace("+00:00", "Z"),
            "status": "partial" if self.errors else "complete",
            "filesystem": {
                "freeBytes": free_bytes,
                "minimumFreeBytes": self.min_free_bytes,
                "belowMinimum": free_bytes < self.min_free_bytes if free_bytes is not None else None,
            },
            "runtime": {"docker": docker, "containerd": containerd},
            "categories": {
                **cache_totals,
                "cacheStatus": cache_statuses,
                "logBytes": log_bytes,
                "logStatus": log_status,
                "totalsAreNonAdditive": True,
            },
            "candidates": candidates,
            "errors": sorted(set(self.errors)),
        }
        return result


def main() -> int:
    try:
        collector = Collector()
        result = collector.collect()
    except (ValueError, OSError) as error:
        print(json.dumps({"schemaVersion": 1, "status": "error", "error": str(error)}, sort_keys=True))
        return 2
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))
    return 1 if result["status"] == "partial" else 0


if __name__ == "__main__":
    sys.exit(main())
