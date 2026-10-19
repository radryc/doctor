import { useEffect, useState } from "react";
import {
  THEMES,
  applyTheme,
  readStoredTheme,
  type Theme,
} from "../../lib/theme";

export default function ThemePicker() {
  const [theme, setTheme] = useState<Theme>(() => readStoredTheme());

  useEffect(() => {
    applyTheme(theme, false);
  }, [theme]);

  return (
    <label className="theme-picker" title="Color theme">
      <span className="theme-picker-swatch" aria-hidden="true" />
      <select
        aria-label="Color theme"
        value={theme}
        onChange={(event) => {
          const next = event.target.value as Theme;
          setTheme(next);
          applyTheme(next);
        }}
      >
        {THEMES.map((item) => (
          <option key={item.id} value={item.id}>
            {item.label}
          </option>
        ))}
      </select>
    </label>
  );
}
