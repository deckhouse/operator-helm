#!/usr/bin/env python3

# Copyright 2023 Flant JSC
# Licensed under the Deckhouse Platform Enterprise Edition (EE) license. See https://github.com/deckhouse/deckhouse/blob/main/ee/LICENSE

import argparse
import fnmatch
import functools
import glob
import json
import logging
import os
import re
import shutil
import subprocess
import sys
import urllib.error
import urllib.request

FORMAT = "{ #'#commit#'#: #'#%H#'#, #'#abbreviated_commit#'#: #'#%h#'#, #'#tree#'#: #'#%T#'#, #'#abbreviated_tree#'#: #'#%t#'#, #'#parent#'#: #'#%P#'#, #'#abbreviated_parent#'#: #'#%p#'#, #'#refs#'#: #'#%D#'#, #'#encoding#'#: #'#%e#'#, #'#subject#'#: #'#%s#'#, #'#sanitized_subject_line#'#: #'#%f#'#, #'#body#'#: #'#%b#'#, #'#commit_notes#'#: #'#%N#'#, #'#verification_flag#'#: #'#%G?#'#, #'#signer#'#: #'#%GS#'#, #'#signer_key#'#: #'#%GK#'#, #'#author#'#: { #'#name#'#: #'#%aN#'#, #'#email#'#: #'#%aE#'#, #'#date#'#: #'#%aD#'# }, #'#commiter#'#: { #'#name#'#: #'#%cN#'#, #'#email#'#: #'#%cE#'#, #'#date#'#: #'#%cD#'# }},"

TRANSLATIONS = {
    "title": {"en": "Release Notes", "ru": "Релизы"},
    "description": {
        "en": "Release notes for Deckhouse operator-helm.",
        "ru": "Релизы Deckhouse operator-helm.",
    },
    "features": {"en": "New Features", "ru": "Новые возможности"},
    "fixes": {"en": "Bug Fixes", "ru": "Исправления"},
    "security": {"en": "Security Fixes", "ru": "Исправления безопасности"},
    "chore": {"en": "Chore", "ru": "Прочее"},
    "breaking": {"en": "Breaking Changes", "ru": "Несовместимые изменения"},
}


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Changelog and Release Notes generator.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument(
        "--config",
        "-c",
        default=os.path.abspath(os.path.join(os.path.dirname(__file__), "chlog.json")),
        type=str,
        help="Config file.",
    )
    parser.add_argument(
        "--verbose", "-v", action="store_true", help="Verbose log output."
    )
    parser.add_argument("--output", "-o", type=str, help="Output file.")

    subparsers = parser.add_subparsers(
        title="subcommands", dest="subcommand", required=True
    )

    parser_changelog = subparsers.add_parser(
        "changelog",
        help="Generate changelog yaml from git commits.",
        description="Generate changelog yaml from git commits.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser_changelog.add_argument(
        "--source", "-s", type=str, required=True, help="Git source revision."
    )
    parser_changelog.add_argument(
        "--target", "-t", default="HEAD", type=str, help="Git target revision."
    )

    parser_release_notes = subparsers.add_parser(
        "release-notes",
        help="Generate release notes markdown from changelogs.",
        description="Generate release notes markdown from changelogs.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser_release_notes.add_argument(
        "--directory",
        "-d",
        default=os.path.abspath(
            os.path.join(os.path.dirname(__file__), "../CHANGELOG/")
        ),
        type=str,
        help="Changelog directory with yaml files.",
    )
    parser_release_notes.add_argument(
        "--lang", "-l", type=str, required=True, help="Target language."
    )

    parser_release_notes = subparsers.add_parser(
        "translate",
        help="Translate file to specified language using Yandex Translate API.",
        description="Translate file to specified language using Yandex Translate API.",
        epilog="IAM token is required.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser_release_notes.add_argument(
        "--file", "-f", type=str, required=True, help="File to translate."
    )
    parser_release_notes.add_argument(
        "--lang", "-l", default="en", type=str, help="Target language."
    )

    parsed_args = parser.parse_args()

    logging.basicConfig(
        format="%(levelname)s: %(message)s",
        level=logging.DEBUG if parsed_args.verbose else logging.INFO,
        stream=sys.stderr,
    )

    if parsed_args.subcommand == "changelog":
        return changelog_handler(
            parsed_args.config,
            parsed_args.source,
            parsed_args.target,
            parsed_args.output,
        )

    if parsed_args.subcommand == "release-notes":
        return release_notes_handler(
            parsed_args.directory,
            parsed_args.lang,
            parsed_args.output,
        )

    if parsed_args.subcommand == "translate":
        return openwebui_translate_handler(
            parsed_args.file, parsed_args.lang, parsed_args.output
        )

    return 0


def ensure_yq(func):
    """Decorator used to check if yq utility is installed"""

    @functools.wraps(func)
    def wrapper(*args, **kwargs):
        if not shutil.which("yq"):
            logging.error(
                "yq is required. Please install https://github.com/mikefarah/yq/#install"
            )
            sys.exit(1)

        func(*args, **kwargs)

    return wrapper


def ensure_yc(func):
    """Decorator used to check if Yandex Cloud CLI is installed"""

    @functools.wraps(func)
    def wrapper(*args, **kwargs):
        if not shutil.which("yc"):
            logging.error(
                "yc is required. Please install https://yandex.cloud/en/docs/cli/operations/install-cli"
            )
            sys.exit(1)
        func(*args, **kwargs)

    return wrapper


@ensure_yq
def changelog_handler(
    config_file: str, source: str, target: str, output: str = ""
) -> int:
    commits = parse_log(source, target)
    mapped = {"features": [], "fixes": [], "security": [], "chore": []}

    # read configuration here because we need to modify the config dict
    with open(config_file, "r", encoding="utf-8") as f:
        config = json.load(f)

    # init components
    for c in config["components"]:
        c["_patterns"] = [re.compile(fnmatch.translate(p)) for p in c["paths"]]

    skip_next = False
    for i, _ in enumerate(commits):
        if skip_next:
            skip_next = False
            continue

        master_entry = commits[i]

        if len(master_entry["parents"]) == 1 or len(commits) < i + 2:
            message = build_message(master_entry["subject"], master_entry["body"])
            change_type = categorize(master_entry["body"])
            components = module_components(config["components"], master_entry["commit"])
            entries = [master_entry]
        else:
            skip_next = True

            dev_entry = commits[i + 1]
            message = build_message(
                dev_entry["subject"],
                master_entry["subject"],
                dev_entry["body"],
                master_entry["body"],
            )
            change_type = categorize(message)
            components = module_components(config["components"], dev_entry["commit"])
            entries = [dev_entry, master_entry]

        mapped[change_type].append(
            fulfill_message(
                {
                    "message": message,
                    "changeType": change_type,
                    "components": components,
                    "entries": entries,
                }
            )
        )

    for changes in mapped.values():
        changes.sort()

    try:
        changelog_yaml = (
            subprocess.check_output(
                ["yq", "-P", "-p", "json", "-o", "yaml", "-"],
                input=json.dumps(mapped, ensure_ascii=False).encode(),
            )
            .decode()
            .strip()
        )
    except subprocess.CalledProcessError as e:
        logging.error(e.stderr.decode().strip())
        return 1

    if output:
        with open(output, "w", encoding="utf-8") as f:
            print(changelog_yaml, file=f)
    else:
        print(changelog_yaml)

    return 0


@ensure_yq
def release_notes_handler(changelog_dir: str, lang: str, output: str = "") -> int:
    if lang not in ["en", "ru"]:
        logging.error('language must be either "en" or "ru".')
        return 1

    changelog_files = sorted(
        glob.glob(pathname=os.path.join(changelog_dir, "*.yaml")), key=lambda f: [int(n) for n in re.findall(r'\d+', f)], reverse=True
    )

    if len(changelog_files) == 0:
        logging.error("no changelog files found.")
        return 1

    changelogs = {}
    for filename in changelog_files:
        logging.debug("processing file: %s", filename)

        if lang == "ru" and not filename.endswith(".ru.yaml"):
            continue

        if lang == "en" and filename.endswith(".ru.yaml"):
            continue

        try:
            changelog_yaml = (
                subprocess.check_output(
                    ["yq", "-o", "json", filename], stderr=subprocess.PIPE
                )
                .decode()
                .strip()
            )
        except subprocess.CalledProcessError as e:
            logging.error(e.stderr.decode().strip())
            return 1

        changelogs[filename] = json.loads(changelog_yaml)

    headers = {
        "title": TRANSLATIONS["title"][lang],
        "breaking": TRANSLATIONS["breaking"][lang],
        "features": TRANSLATIONS["features"][lang],
        "fixes": TRANSLATIONS["fixes"][lang],
        "security": TRANSLATIONS["security"][lang],
        "chore": TRANSLATIONS["chore"][lang],
    }

    markdown = f'---\ntitle: "{headers["title"]}"\ndescription: "{TRANSLATIONS["description"][lang]}"\n---\n'

    for filename, changelog in changelogs.items():
        entries = {
            "version": os.path.basename(filename).replace(
                ".ru.yaml" if lang == "ru" else ".yaml", ""
            ),
            "breaking": "",
            "features": "",
            "fixes": "",
            "security": "",
            "chore": "",
        }

        for category, changes in changelog.items():
            logging.debug("processing category: %s", category)
            if changes:
                if lang == "en" and isinstance(changes, list):
                    changes = [
                        to_past_tense(c) if isinstance(c, str) else c
                        for c in changes
                    ]
                entries[category] = yaml_to_markdown(changes)

        markdown += f"\n## {entries['version']}\n"
        for category in ["breaking", "features", "fixes", "security", "chore"]:
            if entries[category]:
                markdown += f"\n### {headers[category]}\n{entries[category]}"

    if output:
        with open(output, "w", encoding="utf-8") as f:
            print(markdown, file=f)
    else:
        print(markdown)

    return 0


# Leading verbs converted to past tense when rendering release notes.
# Changelog yaml files keep the original (imperative) wording.
PAST_TENSE = {
    "add": "added",
    "allow": "allowed",
    "apply": "applied",
    "bump": "bumped",
    "change": "changed",
    "create": "created",
    "delete": "deleted",
    "disable": "disabled",
    "drop": "dropped",
    "enable": "enabled",
    "enforce": "enforced",
    "fix": "fixed",
    "forbid": "forbidden",
    "improve": "improved",
    "introduce": "introduced",
    "mark": "marked",
    "move": "moved",
    "remove": "removed",
    "rename": "renamed",
    "replace": "replaced",
    "resolve": "resolved",
    "rework": "reworked",
    "update": "updated",
    "upgrade": "upgraded",
}


def to_past_tense(entry: str) -> str:
    words = entry.split(maxsplit=1)
    if not words:
        return entry
    verb = PAST_TENSE.get(words[0].lower())
    if not verb:
        return entry
    return verb + (" " + words[1] if len(words) > 1 else "")


def openwebui_translate_handler(file: str, lang: str, output: str = "") -> int:
    api_token = os.environ.get("GPT_API_TOKEN", None)
    if not api_token:
        logging.error("GPT_API_TOKEN is required.")
        return 1

    api_url = os.environ.get("GPT_API_URL", None)
    if not api_url:
        logging.error("GPT_API_URL is required.")
        return 1

    if lang not in ["en", "ru"]:
        logging.error('language must be either "en" or "ru".')
        return 1

    logging.debug("translating file: %s", file)

    with open(file, "r", encoding="utf-8") as f:
        content = f.read()

    target = "Russian" if lang == "ru" else "English"

    text = (
        f"Translate the following YAML to {target}. Preserve YAML formatting.\n"
        "Do not translate YAML keys. Keep technical terms, product names, CVE ids\n"
        "and file paths in English. Describe changes as already done: use past\n"
        "tense passive forms (e.g. \u00abисправлено\u00bb, \u00abдобавлена\u00bb, \u00abобновлены\u00bb).\n"
        f"\n```{content}```"
    )

    data = {
        "model": "gpt-4o",
        "messages": [
            {
                "role": "user",
                "content": text,
            }
        ],
    }

    req = urllib.request.Request(
        f"{api_url.rstrip('/')}/api/chat/completions",
        headers={
            "Authorization": f"Bearer {api_token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )

    try:
        logging.info("performing request to %s", req.get_full_url())
        with urllib.request.urlopen(req, data=json.dumps(data).encode()) as resp:
            result = json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        logging.error("%d %s: %s", e.code, e.reason, e.read().decode().strip())
        return 1

    if len(result["choices"]) == 0:
        logging.error("something went wrong. no translations found in response")
        return 1

    text = re.sub(
        pattern=r"```(yaml)?(.*)```",
        repl=r"\2",
        string=result["choices"][0]["message"]["content"],
        flags=re.S,
    ).strip()
    if output:
        with open(output, "w", encoding="utf-8") as f:
            print(text, file=f)
    else:
        print(text)

    return 0


@ensure_yc
def yandex_translate_handler(file: str, lang: str, output: str = "") -> int:
    iam_token = os.environ.get("YC_TOKEN", None)
    if not iam_token:
        logging.error(
            'YC_TOKEN is required. Run command: export YC_TOKEN="$(yc iam create-token)"'
        )
        return 1

    folder_id = os.environ.get("YC_FOLDER_ID", None)
    if not folder_id:
        logging.error(
            'YC_FOLDER_ID is required. Run command: export YC_FOLDER_ID="$(yc config get folder-id)"'
        )
        return 1

    if lang not in ["en", "ru"]:
        logging.error('language must be either "en" or "ru".')
        return 1

    logging.debug("translating file: %s", file)

    with open(file, "r", encoding="utf-8") as f:
        content = f.read()

    data = {
        "folderId": folder_id,
        "targetLanguageCode": lang,
        "texts": [content],
    }

    req = urllib.request.Request(
        "https://translate.api.cloud.yandex.net/translate/v2/translate",
        headers={
            "Authorization": f"Bearer {iam_token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(req, data=json.dumps(data).encode()) as resp:
            result = json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        logging.error("%d %s: %s", e.code, e.reason, e.read().decode().strip())
        return 1

    if len(result["translations"]) == 0:
        logging.error("something went wrong. no translations found in response")
        return 1

    text = result["translations"][0]["text"]
    if output:
        with open(output, "w", encoding="utf-8") as f:
            print(text, file=f)
    else:
        print(text)

    return 0


def categorize(body: str) -> str:
    if re.search(r"[Ff]eat(?:ure)?", body):
        return "features"
    if re.search(r"([Ff]ix)|([Bb]ug)", body):
        return "fixes"

    return "chore"


def module_components(config: list[dict], commit: dict) -> list[str]:
    try:
        stdout = (
            subprocess.check_output(
                ["git", "show", "--name-only", "--oneline", "--pretty=format:", commit],
                stderr=subprocess.PIPE,
            )
            .decode()
            .strip()
        )
    except subprocess.CalledProcessError as e:
        logging.error(e.stderr.decode().strip())
        sys.exit(1)

    # use dict to preserve order and remove duplicates
    components = {}

    # init components order from config
    for c in config:
        components[c["name"]] = False

    for line in stdout.splitlines():
        if not line:
            components[""] = True
            continue

        for c in config:
            for r in c["_patterns"]:
                if r.match(line):
                    components[c["name"]] = True
                    break

    # if len(components) > 1:
    #     components.pop("ci", None)
    # if len(components) > 1:
    #     components.pop("module", None)
    # if len(components) > 1:
    #     components.pop("docs", None)

    return [k for k, v in components.items() if v]


def clean_message(message: str) -> list[str]:
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    result = []
    for s in message.splitlines():
        s = s.replace("into 'master'", "")
        s = s.replace("into 'main'", "")
        s = s.replace("Merge branch", "")
        s = s.replace("Merge pull request", "")
        if repo:
            s = re.sub(r"#(\d+)", rf"https://github.com/{repo}/pull/\1", s)
        s = re.sub(r"\s+", " ", s)
        s = re.sub(r"\n", "; ", s)
        s = s.strip()
        result.append(s)

    return result


def build_message(*strings: str) -> str:
    return " ".join(
        dict.fromkeys(message for s in strings for message in clean_message(s))
    )


def fulfill_message(change: dict) -> str:
    author = change["entries"][0]["author"]["email"]
    components = "".join([f"[{c}]" for c in change["components"]])
    commit = change["entries"][0]["abbreviated_commit"]

    return f"{components} <{author}> {change['message']} ({commit})"


def parse_log(source: str, target: str) -> dict:
    try:
        output = (
            subprocess.check_output(
                ["git", "log", f"{source}..{target}", f"--pretty=format:{FORMAT}"],
                stderr=subprocess.PIPE,
            )
            .decode()
            .strip()
        )
    except subprocess.CalledProcessError as e:
        logging.error(e.stderr.decode().strip())
        sys.exit(1)

    output = re.sub(r"\"", '\\"', output)
    output = re.sub(r"#'#", '"', output)
    output = re.sub(r"([^,])\n", r"\1\\n", output)
    output = re.sub(r"([^,])\n", r"\1\\n", output)
    output = re.sub(r"([^,])\n", r"\1\\n", output)
    output = re.sub(r"([^,])\n", r"\1\\n", output)

    commits = json.loads(f"[{output[0:-1]}]")

    for commit in commits:
        commit["parents"] = [c.strip() for c in commit["parent"].split()]
        commit["abbreviated_parents"] = [
            c.strip() for c in commit["abbreviated_parent"].split()
        ]

    return commits


def yaml_to_markdown(yaml_data):
    def parse_dict(d, level=4):
        md = ""
        if isinstance(d, dict):
            for k, v in d.items():
                logging.debug("processing dict: %s", k)
                md += f"\n{'#' * (level)} {k}\n"
                md += parse_dict(v, level + 1)
        elif isinstance(d, list):
            # logging.debug("processing list: %s", d)
            md += "\n"
            for item in d:
                if not isinstance(item, str):
                    logging.error(
                        "invalid value type, expected string: %s: %s", type(item), item
                    )
                    sys.exit(1)
                md += f"* {item}\n"
        return md

    return parse_dict(yaml_data)


if __name__ == "__main__":
    sys.exit(main())
