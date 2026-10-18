package web

import "html/template"

// icons are line icons on a 24-unit grid, drawn with the stroke the
// stylesheet gives .icon, so they take the colour of the text around them.
var icons = map[string]string{
	"agents":   `<rect x="4" y="8" width="16" height="12" rx="3"/><path d="M12 4.5V8M9 13.25v1.5M15 13.25v1.5"/><circle cx="12" cy="3.5" r="1"/>`,
	"chat":     `<path d="M5 4.5h14a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2h-8.5L6 21v-3.5H5a2 2 0 0 1-2-2v-9a2 2 0 0 1 2-2z"/>`,
	"settings": `<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>`,
	"users":    `<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20a6.5 6.5 0 0 1 13 0M15.5 4.6a3.5 3.5 0 0 1 0 6.8M18 14.2a6.5 6.5 0 0 1 3.5 5.8"/>`,
	"audit":    `<path d="M9 6h11M9 12h11M9 18h11M4.5 6h.01M4.5 12h.01M4.5 18h.01"/>`,
	"logout":   `<path d="M9 20H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h3M16 16l4-4-4-4M20 12H9"/>`,
	"plus":     `<path d="M12 5v14M5 12h14"/>`,
	"terminal": `<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7.5 9.5 10 12l-2.5 2.5M13 15h3.5"/>`,
	"wake":     `<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4"/>`,
	"sleep":    `<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9z"/>`,
	"stop":     `<rect x="6" y="6" width="12" height="12" rx="2"/>`,
	"restart":  `<path d="M3.5 12a8.5 8.5 0 0 1 14.6-5.9L20.5 8.5M20.5 3.5v5h-5M20.5 12a8.5 8.5 0 0 1-14.6 5.9L3.5 15.5M3.5 20.5v-5h5"/>`,
	"copy":     `<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15h-.5A1.5 1.5 0 0 1 3 13.5v-9A1.5 1.5 0 0 1 4.5 3h9A1.5 1.5 0 0 1 15 4.5V5"/>`,
	"trash":    `<path d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4.5A1.5 1.5 0 0 1 10.5 3h3A1.5 1.5 0 0 1 15 4.5V7"/>`,
	"link":     `<path d="M10 13a4.5 4.5 0 0 0 6.4.4l3-3a4.5 4.5 0 0 0-6.4-6.4l-1.2 1.2M14 11a4.5 4.5 0 0 0-6.4-.4l-3 3a4.5 4.5 0 0 0 6.4 6.4l1.2-1.2"/>`,
	"back":     `<path d="M19 12H5M11 18l-6-6 6-6"/>`,
	"chevron":  `<path d="M9 6l6 6-6 6"/>`,
	"more":     `<circle cx="5" cy="12" r="1.25" fill="currentColor"/><circle cx="12" cy="12" r="1.25" fill="currentColor"/><circle cx="19" cy="12" r="1.25" fill="currentColor"/>`,
	"check":    `<path d="M5 12.5l4.5 4.5L19 7.5"/>`,
	"circle":   `<circle cx="12" cy="12" r="8.5"/>`,
	"done":     `<circle cx="12" cy="12" r="9"/><path d="M8 12.5l2.8 2.8 5.2-5.8"/>`,
	"key":      `<circle cx="8" cy="15" r="4"/><path d="M10.9 12.1 20 3M16 7l3 3M18.5 4.5l2 2"/>`,
	"lock":     `<rect x="4.5" y="10.5" width="15" height="10" rx="2"/><path d="M8 10.5v-3a4 4 0 0 1 8 0v3"/>`,
	"alert":    `<circle cx="12" cy="12" r="9"/><path d="M12 7.5v5.5M12 16.5h.01"/>`,
	"info":     `<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5M12 7.5h.01"/>`,
	"send":     `<path d="M4.5 12 20 4.5 13.5 20l-2.5-6.5zM11 13.5 20 4.5"/>`,
	"upgrade":  `<circle cx="12" cy="12" r="9"/><path d="M12 16V8M8.5 11.5 12 8l3.5 3.5"/>`,
	"phone":    `<rect x="6.5" y="2.5" width="11" height="19" rx="2.5"/><path d="M11 18.5h2"/>`,
	"qr":       `<rect x="4" y="4" width="6" height="6" rx="1"/><rect x="14" y="4" width="6" height="6" rx="1"/><rect x="4" y="14" width="6" height="6" rx="1"/><path d="M14 14h2.5v2.5H14zM17.5 17.5H20V20h-2.5zM14 20h.01M20 14h.01"/>`,
	"user":     `<circle cx="12" cy="8" r="4"/><path d="M4.5 20.5a7.5 7.5 0 0 1 15 0"/>`,
	"x":        `<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>`,
	"external": `<path d="M14 4h6v6M20 4l-9 9M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4"/>`,
}

func icon(name string) template.HTML {
	return template.HTML(`<svg class="icon" viewBox="0 0 24 24" aria-hidden="true">` + icons[name] + `</svg>`)
}
