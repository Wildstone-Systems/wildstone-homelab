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
from typing import Any, Callable


RESTIC_IMAGE = (
    "restic/restic@sha256:9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481"
)
DOCKER = ["docker", "--host", "unix:///var/run/docker.sock"]
CONTAINERD_ARGS = [
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


class ConfigurationError(ValueError):
    """A privacy-safe configuration failure with a fixed public code."""


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
            raise ConfigurationError(f"{name.lower()}_invalid")
        return int(value, 10)

    def positive_int(self, name: str, default: int) -> int:
        value = self.nonnegative_int(name, default)
        if value == 0:
            raise ConfigurationError(f"{name.lower()}_invalid")
        return value

    def timestamp(self) -> dt.datetime:
        configured = os.environ.get("REPORT_TIMESTAMP")
        if configured:
            try:
                parsed = dt.datetime.fromisoformat(configured.replace("Z", "+00:00"))
            except (ValueError, OverflowError) as error:
                raise ConfigurationError("report_timestamp_invalid") from error
            if parsed.tzinfo is None:
                raise ConfigurationError("report_timestamp_invalid")
            try:
                return parsed.astimezone(dt.timezone.utc)
            except (ValueError, OverflowError) as error:
                raise ConfigurationError("report_timestamp_invalid") from error
        return dt.datetime.now(dt.timezone.utc)

    def path(self, absolute: str) -> Path:
        return self.root / absolute.lstrip("/")

    def tree_scan(
        self,
        absolute: str,
        error_code: str,
        *,
        include_files: bool = False,
        file_name_filter: Callable[[str], bool] | None = None,
        count_directories: bool = True,
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
            if not statmode.S_ISDIR(root_stat.st_mode):
                raise NotADirectoryError
            if count_directories:
                total += root_stat.st_blocks * 512
            seen_inodes = {(root_stat.st_dev, root_stat.st_ino)}
            pending = [path]
            while pending:
                current = pending.pop()
                with os.scandir(current) as entries:
                    for entry in entries:
                        entry_stat = entry.stat(follow_symlinks=False)
                        inode = (entry_stat.st_dev, entry_stat.st_ino)
                        if statmode.S_ISDIR(entry_stat.st_mode):
                            if inode in seen_inodes:
                                continue
                            seen_inodes.add(inode)
                            if count_directories:
                                total += entry_stat.st_blocks * 512
                            if entry_stat.st_dev == root_device:
                                pending.append(Path(entry.path))
                            continue
                        is_regular = statmode.S_ISREG(entry_stat.st_mode)
                        if file_name_filter is not None and (
                            not is_regular or not file_name_filter(entry.name)
                        ):
                            continue
                        if inode in seen_inodes:
                            continue
                        seen_inodes.add(inode)
                        total += entry_stat.st_blocks * 512
                        if include_files and is_regular:
                            files.append((entry_stat.st_blocks * 512, entry_stat.st_mtime))
        except (OSError, PermissionError):
            self.errors.append(error_code)
            return total, "incomplete", []
        return total, "complete", files

    def tree_bytes(self, absolute: str, error_code: str) -> tuple[int, str]:
        total, status, _ = self.tree_scan(absolute, error_code)
        return total, status

    @staticmethod
    def docker_log_name(name: str) -> bool:
        container_id, marker, rotation = name.partition("-json.log")
        return bool(container_id and marker) and (
            not rotation or (rotation.startswith(".") and rotation[1:].isdigit())
        )

    def docker_log_bytes(self) -> tuple[int, str]:
        total, status, _ = self.tree_scan(
            "/var/lib/docker/containers",
            "docker_log_storage_unreadable",
            file_name_filter=self.docker_log_name,
            count_directories=False,
        )
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

    @staticmethod
    def containerd_client() -> list[str] | None:
        if shutil.which("ctr") is not None:
            return ["ctr", *CONTAINERD_ARGS]
        if shutil.which("k3s") is not None:
            return ["k3s", "ctr", *CONTAINERD_ARGS]
        return None

    @staticmethod
    def nonempty_lines(output: str) -> list[str]:
        return [line.strip() for line in output.splitlines() if line.strip()]

    def docker_images(self, output: str | None) -> tuple[set[str], dict[str, str], bool]:
        """Return unique image IDs and their public references from JSON lines."""
        if output is None:
            return set(), {}, False
        images: set[str] = set()
        references: dict[str, str] = {}
        try:
            rows = [json.loads(line) for line in self.nonempty_lines(output)]
        except json.JSONDecodeError:
            self.errors.append("docker_image_inventory_malformed")
            return set(), {}, False
        for row in rows:
            if not isinstance(row, dict):
                self.errors.append("docker_image_inventory_malformed")
                return set(), {}, False
            image_id = row.get("ID")
            repository = row.get("Repository")
            tag = row.get("Tag")
            digest = row.get("Digest")
            if not image_id or not all(
                isinstance(value, str) for value in (image_id, repository, tag, digest)
            ):
                self.errors.append("docker_image_inventory_malformed")
                return set(), {}, False
            images.add(image_id)
            if repository != "<none>" and tag != "<none>":
                reference = f"{repository}:{tag}"
                if reference in references and references[reference] != image_id:
                    self.errors.append("docker_image_inventory_malformed")
                    return set(), {}, False
                references[reference] = image_id
            if repository != "<none>" and digest != "<none>":
                reference = f"{repository}@{digest}"
                if reference in references and references[reference] != image_id:
                    self.errors.append("docker_image_inventory_malformed")
                    return set(), {}, False
                references[reference] = image_id
        return images, references, True

    @staticmethod
    def valid_digest(value: str) -> bool:
        algorithm, separator, encoded = value.partition(":")
        return (
            bool(separator)
            and algorithm.replace("-", "").replace("_", "").isalnum()
            and encoded.isalnum()
        )

    def containerd_images(self, output: str | None) -> tuple[dict[str, str], bool]:
        """Map containerd aliases to target digests without exposing either."""
        if output is None:
            return {}, False
        lines = self.nonempty_lines(output)
        if not lines or lines[0].split()[:3] != ["REF", "TYPE", "DIGEST"]:
            self.errors.append("containerd_image_inventory_malformed")
            return {}, False
        images: dict[str, str] = {}
        for line in lines[1:]:
            fields = line.split()
            if len(fields) < 3 or not self.valid_digest(fields[2]):
                self.errors.append("containerd_image_inventory_malformed")
                return {}, False
            image_ref, digest = fields[0], fields[2]
            if image_ref in images and images[image_ref] != digest:
                self.errors.append("containerd_image_inventory_malformed")
                return {}, False
            images[image_ref] = digest
        return images, True

    @staticmethod
    def protected_containerd_identities(
        protected_refs: list[str], images: dict[str, str]
    ) -> set[str]:
        identities = set(images.values())
        protected: set[str] = set()
        for image_ref in protected_refs:
            if image_ref in images:
                protected.add(images[image_ref])
            _, separator, digest = image_ref.rpartition("@")
            if separator and digest in identities:
                protected.add(digest)
        return protected

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
            [
                *DOCKER,
                "image",
                "ls",
                "--no-trunc",
                "--digests",
                "--format",
                "{{json .}}",
            ],
            "docker_image_inventory_failed",
        )
        containers_output = self.command(
            [*DOCKER, "ps", "-aq", "--no-trunc"], "docker_container_inventory_failed"
        )
        images, image_refs, images_ok = self.docker_images(images_output)
        protected_refs, protected_ok = self.protected_images()
        references: set[str] = set()
        references_ok = containers_output is not None and protected_ok and images_ok
        if containers_output is not None:
            for container_id in self.nonempty_lines(containers_output):
                image_id = self.command(
                    [*DOCKER, "container", "inspect", "--format", "{{.Image}}", container_id],
                    "docker_container_reference_failed",
                )
                if image_id is None:
                    references_ok = False
                elif len(self.nonempty_lines(image_id)) != 1:
                    self.errors.append("docker_container_reference_malformed")
                    references_ok = False
                else:
                    references.add(self.nonempty_lines(image_id)[0])
        for image_ref in protected_refs:
            if image_ref in image_refs:
                references.add(image_refs[image_ref])

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
            images_ok
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
            "imageCount": len(images) if images_ok else None,
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
        containerd_client = self.containerd_client()
        if containerd_client is None:
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
            [*containerd_client, "images", "list"],
            "containerd_image_inventory_failed",
        )
        containers_output = self.command(
            [*containerd_client, "containers", "list", "-q"],
            "containerd_container_inventory_failed",
        )
        images, images_ok = self.containerd_images(images_output)
        identities = set(images.values())
        protected_refs, protected_ok = self.protected_images()
        references: set[str] = set()
        references_ok = containers_output is not None and protected_ok and images_ok
        if containers_output is not None:
            for container_id in self.nonempty_lines(containers_output):
                info = self.command(
                    [*containerd_client, "containers", "info", container_id],
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
                elif image_ref not in images:
                    self.errors.append("containerd_reference_unknown")
                    references_ok = False
                else:
                    references.add(images[image_ref])

        references.update(self.protected_containerd_identities(protected_refs, images))
        complete = (
            images_ok
            and references_ok
            and image_status != "incomplete"
            and snapshot_status != "incomplete"
        )
        referenced_count = len(identities & references) if complete else None
        return {
            "presence": "present",
            "collectionStatus": "complete" if complete else "incomplete",
            "imageBytes": image_bytes,
            "imageStorageStatus": image_status,
            "snapshotBytes": snapshot_bytes,
            "snapshotStorageStatus": snapshot_status,
            "imageCount": len(identities) if images_ok else None,
            "referencedImageCount": referenced_count,
            "unreferencedImageCount": len(identities) - referenced_count if complete else None,
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
        docker_log_bytes, docker_log_status = self.docker_log_bytes()
        result = {
            "schemaVersion": 1,
            "collectedAt": self.now.isoformat().replace("+00:00", "Z"),
            "status": "partial" if self.errors else "complete",
            "retentionDays": self.retention_days,
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
                "dockerContainerLogBytes": docker_log_bytes,
                "dockerContainerLogStatus": docker_log_status,
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
    except ConfigurationError as error:
        print(json.dumps({"schemaVersion": 1, "status": "error", "error": str(error)}, sort_keys=True))
        return 2
    except (ValueError, OSError, OverflowError):
        print(
            json.dumps(
                {
                    "schemaVersion": 1,
                    "status": "error",
                    "error": "collector_initialization_failed",
                },
                sort_keys=True,
            )
        )
        return 2
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))
    return 1 if result["status"] == "partial" else 0


if __name__ == "__main__":
    sys.exit(main())
