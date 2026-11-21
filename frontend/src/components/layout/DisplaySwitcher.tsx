import { useEffect, useState } from "react";
import { currentDisplayProfile, setDisplayProfile } from "../../lib/display";

interface ProfileOption {
  id: string;
  name?: string;
}

export default function DisplaySwitcher() {
  const [profiles, setProfiles] = useState<ProfileOption[]>([]);
  const [active, setActive] = useState(currentDisplayProfile() || "canonical");

  useEffect(() => {
    let cancelled = false;
    if (!currentDisplayProfile()) {
      fetch("/v1/display-profiles/effective", { headers: { Accept: "application/json" } })
        .then((res) => (res.ok ? res.json() : { profile: "" }))
        .then((body) => {
          if (!cancelled && body.profile) {
            void setDisplayProfile(body.profile).then(() => {
              setActive(body.profile);
            });
          }
        })
        .catch(() => undefined);
    }
    fetch("/v1/display-profiles", { headers: { Accept: "application/json" } })
      .then((res) => (res.ok ? res.json() : { profiles: [] }))
      .then((body) => {
        if (!cancelled) setProfiles(Array.isArray(body.profiles) ? body.profiles : []);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <label className="app-topbar__display">
      <span className="sr-only">Display labels</span>
      <select
        className="app-topbar__select"
        value={active}
        onChange={(event) => {
          const next = event.target.value;
          setActive(next);
          void setDisplayProfile(next).finally(() => window.location.reload());
        }}
      >
        <option value="canonical">Canonical labels</option>
        {profiles.map((profile) => (
          <option key={profile.id} value={profile.id}>
            {profile.name || profile.id}
          </option>
        ))}
      </select>
    </label>
  );
}
