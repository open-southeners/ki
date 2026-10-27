import { describe, expect, test } from "bun:test";
import {
  composeImageServices,
  envRowsError,
  envRowsToMap,
  gitFormToParams,
  imageFormError,
  imageFormToParams,
  imageRefError,
  imageSetTarget,
  parseEnvText,
  volumeRowsToSpecs,
  type ImageForm,
} from "./newproject";

describe("imageRefError", () => {
  test("accepts pinned refs", () => {
    for (const r of ["ghcr.io/acme/web:1.4.2", "nginx:1.27", "localhost:5000/app:v1", `ghcr.io/a/b@sha256:${"a".repeat(64)}`]) {
      expect(imageRefError(r)).toBeNull();
    }
  });
  test("refuses untagged, latest and junk", () => {
    expect(imageRefError("")).not.toBeNull();
    expect(imageRefError("nginx")).not.toBeNull();
    expect(imageRefError("localhost:5000/app")).not.toBeNull();
    expect(imageRefError("nginx:latest")).not.toBeNull();
    expect(imageRefError("a b:1")).not.toBeNull();
    expect(imageRefError("a/b@sha256:xyz")).not.toBeNull();
  });
});

describe("imageSetTarget", () => {
  test("tag vs full ref", () => {
    expect(imageSetTarget("1.4.3")).toEqual({ tag: "1.4.3" });
    expect(imageSetTarget("ghcr.io/a/b:2")).toEqual({ image: "ghcr.io/a/b:2" });
    expect("error" in imageSetTarget("latest")).toBe(true);
    expect("error" in imageSetTarget("ghcr.io/a/b")).toBe(true);
  });
});

describe("env", () => {
  test("parse and map", () => {
    const rows = parseEnvText("# c\nA=1\n\nB=x=y\nC");
    expect(rows).toEqual([
      { key: "A", value: "1" },
      { key: "B", value: "x=y" },
      { key: "C", value: "" },
    ]);
    expect(envRowsToMap([...rows, { key: "", value: "skip" }])).toEqual({ A: "1", B: "x=y", C: "" });
  });
  test("validation", () => {
    expect(envRowsError([{ key: "OK_1", value: "" }])).toBeNull();
    expect(envRowsError([{ key: "1BAD", value: "" }])).not.toBeNull();
    expect(envRowsError([{ key: "A", value: "" }, { key: "A", value: "" }])).not.toBeNull();
  });
});

const base: ImageForm = {
  slug: "web",
  image: "ghcr.io/acme/web:1.4.2",
  service: "",
  port: "",
  domains: "",
  path: "",
  volumes: [],
  env: [],
};

describe("image form", () => {
  test("minimal maps to slug and image only", () => {
    expect(imageFormError(base)).toBeNull();
    expect(imageFormToParams(base)).toEqual({ slug: "web", image: { image: "ghcr.io/acme/web:1.4.2" } });
  });
  test("domains need a port", () => {
    expect(imageFormError({ ...base, domains: "a.example.com" })).toBe("Domains need a port");
  });
  test("full mapping", () => {
    const f: ImageForm = {
      ...base,
      service: "web",
      port: "8080",
      domains: "a.example.com, b.example.com",
      path: "/app",
      volumes: [
        { name: "data", path: "/app/data", readOnly: false },
        { name: "cfg", path: "/etc/cfg", readOnly: true },
        { name: "", path: "", readOnly: false },
      ],
      env: [{ key: "MODE", value: "prod" }],
    };
    expect(imageFormError(f)).toBeNull();
    expect(imageFormToParams(f).image).toEqual({
      image: "ghcr.io/acme/web:1.4.2",
      service: "web",
      port: 8080,
      domains: ["a.example.com", "b.example.com"],
      path: "/app",
      volumes: ["data:/app/data", "cfg:/etc/cfg:ro"],
      env: { MODE: "prod" },
    });
  });
  test("rejects bind mounts and bad ports", () => {
    expect(imageFormError({ ...base, volumes: [{ name: "/host", path: "/c", readOnly: false }] })).not.toBeNull();
    expect(imageFormError({ ...base, port: "70000" })).not.toBeNull();
    expect(imageFormError({ ...base, slug: "Bad Slug" })).not.toBeNull();
  });
  test("volumeRowsToSpecs", () => {
    expect(volumeRowsToSpecs([{ name: "d", path: "/d", readOnly: true }])).toEqual(["d:/d:ro"]);
  });
});

describe("git form", () => {
  const g = { repoUrl: " https://github.com/a/b.git ", slug: "", mode: "tag_pattern" as const, ref: "v*", composeFile: "", token: "", tokenUser: "" };
  test("maps trigger and drops blanks", () => {
    expect(gitFormToParams(g)).toEqual({ repo_url: "https://github.com/a/b.git", tag_pattern: "v*" });
  });
  test("token only for https", () => {
    expect(gitFormToParams({ ...g, token: "t", tokenUser: "u" })).toMatchObject({ token: "t", token_user: "u" });
    expect(gitFormToParams({ ...g, repoUrl: "git@github.com:a/b.git", token: "t" })).not.toHaveProperty("token");
  });
});

describe("composeImageServices", () => {
  test("lists services with an image", () => {
    const y = `name: x
services:
  web:
    image: "ghcr.io/a/web:1" # c
    ports: ["80:80"]
  db:
    image: postgres:16
  worker:
    build: .
volumes:
  data:
`;
    expect(composeImageServices(y)).toEqual([
      { name: "web", image: "ghcr.io/a/web:1" },
      { name: "db", image: "postgres:16" },
    ]);
  });
});
