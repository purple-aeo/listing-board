import { FormEvent, useEffect, useState } from "react";

type Step = { label: string; state: string };

type Job = {
  id: string;
  title: string;
  address: string;
  status: string;
  error: string;
  filename: string;
  checklist: Step[];
};

type Account = { id: string; email: string };

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: "include", ...init });
  const text = await res.text();
  let data: any = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    const message = data && typeof data.error === "string" ? data.error : text || res.statusText;
    throw new Error(message);
  }
  return data as T;
}

export function App() {
  const [me, setMe] = useState<Account | null>(null);
  const [ready, setReady] = useState(false);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [selected, setSelected] = useState<Job | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    api<Account>("/api/me")
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setReady(true));
  }, []);

  useEffect(() => {
    if (!me) {
      setJobs([]);
      return;
    }
    api<Job[]>("/api/jobs")
      .then(setJobs)
      .catch((err: Error) => setError(err.message));
  }, [me]);

  useEffect(() => {
    if (!selected || selected.status !== "running") {
      return;
    }

    const id = selected.id;
    let active = true;
    let timer: ReturnType<typeof setInterval> | null = null;

    const poll = () => {
      api<Job>(`/api/jobs/${id}`)
        .then((updated) => {
          if (!active) return;
          setSelected((current) => (current?.id === id ? updated : current));
          setJobs((current) =>
            current.map((item) => (item.id === id ? updated : item))
          );
          if (updated.status !== "running" && timer !== null) {
            clearInterval(timer);
          }
        })
        .catch(() => {
          // Ignore transient polling errors
        });
    };

    poll();
    timer = setInterval(poll, 500);

    return () => {
      active = false;
      if (timer !== null) {
        clearInterval(timer);
      }
    };
  }, [selected?.id, selected?.status]);

  if (!ready) {
    return <p className="muted">Loading…</p>;
  }
  if (!me) {
    return <Auth onIn={setMe} />;
  }

  return (
    <main>
      <header>
        <div>
          <p className="eyebrow">Real estate</p>
          <h1>Listing board</h1>
        </div>
        <button
          className="ghost"
          onClick={() => {
            api("/api/logout", { method: "POST" }).finally(() => {
              setMe(null);
              setSelected(null);
            });
          }}
        >
          Log out
        </button>
      </header>
      {error ? <p className="error">{error}</p> : null}
      <Create
        onCreated={(job) => {
          setJobs((current) => [job, ...current]);
          setSelected(job);
          setError("");
        }}
        onError={setError}
      />
      <section className="layout">
        <ul className="list">
          {jobs.length === 0 ? <li className="muted">No sheets yet.</li> : null}
          {jobs.map((job) => (
            <li key={job.id}>
              <button className={selected?.id === job.id ? "row on" : "row"} onClick={() => setSelected(job)}>
                <span>
                  <strong>{job.title}</strong>
                  <span className="muted">{job.address}</span>
                </span>
                <span className={"status " + job.status}>{job.status}</span>
              </button>
            </li>
          ))}
        </ul>
        {selected ? (
          <Sheet
            job={selected}
            onChange={(job) => {
              setSelected(job);
              setJobs((current) => current.map((item) => (item.id === job.id ? job : item)));
            }}
            onError={setError}
          />
        ) : (
          <p className="muted">Select a sheet.</p>
        )}
      </section>
    </main>
  );
}

function Auth({ onIn }: { onIn: (account: Account) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    try {
      const account = await api<Account>("/api/" + mode, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      onIn(account);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not sign in");
    }
  }

  return (
    <main className="narrow">
      <p className="eyebrow">Real estate</p>
      <h1>Listing board</h1>
      <form onSubmit={submit}>
        <label>
          Email
          <input value={email} onChange={(event) => setEmail(event.target.value)} type="email" required />
        </label>
        <label>
          Password
          <input value={password} onChange={(event) => setPassword(event.target.value)} type="password" required />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <button type="submit">{mode === "login" ? "Sign in" : "Create account"}</button>
      </form>
      <button className="ghost" onClick={() => setMode(mode === "login" ? "register" : "login")}>
        {mode === "login" ? "Need an account?" : "Already have an account?"}
      </button>
    </main>
  );
}

function Create({ onCreated, onError }: { onCreated: (job: Job) => void; onError: (message: string) => void }) {
  const [title, setTitle] = useState("");
  const [address, setAddress] = useState("");
  const [file, setFile] = useState<File | null>(null);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!file) {
      onError("Choose a brochure file");
      return;
    }
    try {
      const job = await api<Job>("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, address }),
      });
      const body = new FormData();
      body.append("file", file);
      const saved = await api<Job>("/api/jobs/" + job.id + "/file", { method: "POST", body });
      setTitle("");
      setAddress("");
      setFile(null);
      onCreated(saved);
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not create the sheet");
    }
  }

  return (
    <form className="create" onSubmit={submit}>
      <label>
        Title
        <input value={title} onChange={(event) => setTitle(event.target.value)} required />
      </label>
      <label>
        Address
        <input value={address} onChange={(event) => setAddress(event.target.value)} required />
      </label>
      <label>
        Brochure
        <input
          type="file"
          accept=".pdf,.txt,application/pdf,text/plain"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
          required
        />
      </label>
      <button type="submit">Create sheet</button>
    </form>
  );
}

function Sheet({
  job,
  onChange,
  onError,
}: {
  job: Job;
  onChange: (job: Job) => void;
  onError: (message: string) => void;
}) {
  async function start() {
    onError("");
    try {
      const next = await api<Job>("/api/jobs/" + job.id + "/start", { method: "POST" });
      onChange(next);
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not start the sheet");
    }
  }

  return (
    <article className="sheet">
      <h2>{job.title}</h2>
      <p>{job.address}</p>
      <p className={"status " + job.status}>{job.status}</p>
      {job.filename ? <p className="muted">{job.filename}</p> : null}
      {job.error ? <p className="error">{job.error}</p> : null}
      <ol>
        {job.checklist.map((step) => (
          <li key={step.label} className={step.state}>
            {step.label}
          </li>
        ))}
      </ol>
      {job.status === "draft" ? <button onClick={start}>Start</button> : null}
    </article>
  );
}
