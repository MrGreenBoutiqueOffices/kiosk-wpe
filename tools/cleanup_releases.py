"""Cleanup script to remove temporary releases from Balena Cloud."""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys

from balena_cloud import BalenaCloud, Release

BALENA_TOKEN = os.getenv("BALENA_TOKEN")
BALENA_FLEET_SLUG = os.getenv("BALENA_FLEET_SLUG")

logging.addLevelName(logging.INFO, "")
logging.addLevelName(logging.ERROR, "::error::")
logging.addLevelName(logging.WARNING, "::warning::")
logging.basicConfig(
    level=logging.INFO,
    format=" %(levelname)s %(message)s",
    handlers=[logging.StreamHandler(sys.stdout)],
)
LOGGER = logging.getLogger(__name__)


async def get_temporary_releases(client: BalenaCloud, pr_number: int) -> list[Release]:
    """Get temporary releases for a given pull request."""
    try:
        fleet = await client.fleet.get(fleet_slug=BALENA_FLEET_SLUG)
        releases = await client.fleet.get_releases(
            fleet_id=fleet.id,
            filters={
                "is_final": False,
                "semver_prerelease": f"pr-{pr_number}",
            },
        )
    except Exception:
        LOGGER.exception("An error occurred while fetching temporary releases.")
        return []
    else:
        return releases


async def delete_release(client: BalenaCloud, release_id: str) -> None:
    """Delete a release from Balena Cloud."""
    try:
        await client.release.remove(release_id)
    except Exception:
        LOGGER.exception("Release %s not found. Skipping deletion.", release_id)
    else:
        LOGGER.info("Deleting release %s", release_id)


async def main() -> None:
    """Run cleanup."""
    parser = argparse.ArgumentParser(description="Cleanup temporary Balena releases.")
    parser.add_argument("--pr-number", required=True, type=int, help="Pull request number.")

    args = parser.parse_args()
    LOGGER.info("Searching for temporary releases for PR #%s in fleet '%s'", args.pr_number, BALENA_FLEET_SLUG)

    async with BalenaCloud(token=BALENA_TOKEN) as client:
        releases = await get_temporary_releases(client, args.pr_number)

        if not releases:
            LOGGER.info("No temporary releases found. Cleanup not needed.")
            return

        await asyncio.gather(*(delete_release(client, release.id) for release in releases))
        LOGGER.info("Cleanup completed. Deleted %s temporary releases.", len(releases))


if __name__ == "__main__":
    asyncio.run(main())
