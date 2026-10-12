/*
 * The CloudGround design system (DESIGN.md, "Rapporto di prova") mapped
 * onto Nuxt UI. Every colour is a token from main.css; nothing here names
 * a colour value.
 *
 * - Plain controls are the default: a button is paper with a 1px
 *   --rule-strong frame (`neutral` + `outline`). `primary` is the orange,
 *   for the one primary action of a screen (Nuovo sito, Aggiorna ora,
 *   Apri wp-admin, a dialog's confirming action), so it is asked for by
 *   name and never a default.
 * - Blue (`secondary`) carries links, focus, selection and the checked
 *   state of form controls, which therefore default to it.
 * - Panels and tables are ruled with 1px lines and square; controls have
 *   a 3px radius. Status badges are an 8px square and a word.
 * - Plex Sans throughout; mono only for commands, figures, paths and
 *   identifiers, never as a label style.
 */
const R = 'rounded-[3px]'
const FOCUS = 'outline-cg-blue focus-visible:outline-2 focus-visible:outline-offset-2'
const POPOVER = `${R} bg-default ring ring-accented shadow-lg`
const MENU_ITEM = 'text-default data-highlighted:text-highlighted data-[state=open]:text-highlighted data-highlighted:before:bg-accented data-[state=open]:before:bg-accented before:rounded-[2px]'
// An empty state's sizes, all one (the empty config below).
const EMPTY = { avatar: 'size-10 text-[40px]', title: 'text-xl', description: 'text-sm' }
// The status badge: an 8px square in the word's colour; hollow when off.
const DOT = 'bg-transparent ring-0 px-0 py-0 before:content-[\'\'] before:size-2 before:shrink-0 before:bg-current has-[>[data-slot=leadingIcon]]:before:hidden'
const SUBTLE = ['soft', 'subtle'] as ('soft' | 'subtle')[]
// Form controls draw their checked and focused states in blue.
const BLUE = { color: 'secondary' as const }

export default defineAppConfig({
  ui: {
    // The semantic variables (--ui-primary, …) are set from the tokens in
    // main.css; these palettes only seed Nuxt UI's own scales.
    colors: {
      primary: 'orange',
      secondary: 'blue',
      neutral: 'stone',
      success: 'green',
      info: 'blue',
      warning: 'amber',
      error: 'red'
    },
    // Nuxt UI ships Lucide defaults; the interface uses the CloudGround set
    // (app/assets/icons) throughout, so every key Nuxt UI 4.11 reads is set
    // here. The set has no double chevrons, sidebar panels, grip, hash, star,
    // monitor or bulb: the closest of its own glyphs stand in for those.
    icons: {
      arrowDown: 'i-cg-arrow-down',
      arrowLeft: 'i-cg-arrow-left',
      arrowRight: 'i-cg-arrow-right',
      arrowUp: 'i-cg-arrow-up',
      caution: 'i-cg-alert-circle',
      check: 'i-cg-check',
      chevronDoubleLeft: 'i-cg-arrow-left',
      chevronDoubleRight: 'i-cg-arrow-right',
      chevronDown: 'i-cg-chevron-down',
      chevronLeft: 'i-cg-chevron-left',
      chevronRight: 'i-cg-chevron-right',
      chevronUp: 'i-cg-chevron-up',
      close: 'i-cg-x',
      copy: 'i-cg-copy',
      copyCheck: 'i-cg-check',
      dark: 'i-cg-moon',
      drag: 'i-cg-more-vertical',
      ellipsis: 'i-cg-more-horizontal',
      error: 'i-cg-x-circle',
      external: 'i-cg-external-link',
      eye: 'i-cg-eye',
      eyeOff: 'i-cg-eye-off',
      file: 'i-cg-file',
      folder: 'i-cg-folder',
      folderOpen: 'i-cg-folder-open',
      hash: 'i-cg-link',
      info: 'i-cg-info',
      light: 'i-cg-sun',
      loading: 'i-cg-loader',
      menu: 'i-cg-menu',
      minus: 'i-cg-minus',
      panelClose: 'i-cg-chevron-left',
      panelOpen: 'i-cg-chevron-right',
      plus: 'i-cg-plus',
      reload: 'i-cg-refresh',
      search: 'i-cg-search',
      star: 'i-cg-verified',
      stop: 'i-cg-stop',
      success: 'i-cg-check-circle',
      system: 'i-cg-browser',
      tip: 'i-cg-info',
      upload: 'i-cg-upload',
      warning: 'i-cg-alert-triangle'
    },

    button: {
      slots: {
        base: `${R} font-medium justify-center text-center transition-colors duration-150`
      },
      variants: {
        size: {
          xs: { base: 'px-2 py-1 text-[13px] gap-1.5 min-h-7' },
          sm: { base: 'px-3 py-1 text-sm gap-1.5 min-h-8' },
          md: { base: 'px-4 py-1.5 text-[15px] gap-2 min-h-10' },
          lg: { base: 'px-5 py-2 text-base gap-2 min-h-11' },
          xl: { base: 'px-6 py-2.5 text-base gap-2.5 min-h-12' }
        }
      },
      compoundVariants: [
        // The one primary action: orange, semibold.
        { color: 'primary', variant: 'solid', class: `bg-cg-orange text-cg-on-orange font-semibold hover:bg-(--cg-orange-hover) active:bg-(--cg-orange-hover) disabled:opacity-100 aria-disabled:opacity-100 disabled:bg-default aria-disabled:bg-default disabled:text-muted aria-disabled:text-muted disabled:outline-1 disabled:outline-dashed disabled:-outline-offset-1 disabled:outline-accented ${FOCUS}` },
        // The plain button: paper, a --rule-strong frame; dashed when disabled.
        { color: ['primary', 'neutral'], variant: 'outline', class: `ring ring-inset ring-accented bg-default text-highlighted hover:bg-accented active:bg-(--cg-rule) disabled:opacity-100 aria-disabled:opacity-100 disabled:bg-default aria-disabled:bg-default disabled:text-muted aria-disabled:text-muted disabled:ring-0 aria-disabled:ring-0 disabled:outline-1 disabled:outline-dashed disabled:-outline-offset-1 disabled:outline-accented ${FOCUS} focus-visible:ring-accented` },
        { color: 'neutral', variant: 'solid', class: `bg-inverted text-inverted hover:bg-(--cg-text) active:bg-(--cg-text) disabled:bg-inverted aria-disabled:bg-inverted ${FOCUS}` },
        { color: ['primary', 'neutral'], variant: SUBTLE, class: `ring-0 text-highlighted bg-elevated hover:bg-accented active:bg-accented disabled:bg-elevated aria-disabled:bg-elevated ${FOCUS}` },
        { color: ['primary', 'neutral'], variant: 'ghost', class: `text-default hover:bg-accented active:bg-accented hover:text-highlighted ${FOCUS}` },
        { color: ['primary', 'neutral', 'secondary'], variant: 'link', class: `px-0 min-h-0 text-cg-blue underline decoration-1 underline-offset-[0.2em] hover:decoration-2 hover:text-cg-blue active:text-cg-blue disabled:text-muted ${FOCUS}` },
        { color: 'secondary', variant: 'solid', class: `bg-cg-blue text-cg-paper hover:bg-inverted hover:text-inverted ${FOCUS}` },
        { color: ['success', 'warning', 'error', 'info', 'secondary'], variant: SUBTLE, class: `ring-0 ${FOCUS}` },
        { color: 'success', variant: SUBTLE, class: 'bg-cg-ok-tint text-cg-ok hover:ring-1 hover:ring-inset hover:ring-cg-ok' },
        { color: 'warning', variant: SUBTLE, class: 'bg-cg-warn-tint text-cg-warn hover:ring-1 hover:ring-inset hover:ring-cg-warn' },
        { color: 'error', variant: SUBTLE, class: 'bg-cg-err-tint text-cg-err hover:ring-1 hover:ring-inset hover:ring-cg-err' },
        { color: ['secondary', 'info'], variant: SUBTLE, class: 'bg-cg-blue-tint text-cg-blue hover:ring-1 hover:ring-inset hover:ring-cg-blue' },
        { color: 'error', variant: 'solid', class: `bg-cg-err text-cg-paper hover:bg-cg-err/90 active:bg-cg-err/90 disabled:bg-cg-err ${FOCUS}` },
        { color: 'warning', variant: 'solid', class: `bg-cg-warn text-cg-paper hover:bg-cg-warn/90 active:bg-cg-warn/90 disabled:bg-cg-warn ${FOCUS}` },
        { color: 'error', variant: 'outline', class: `ring-cg-err text-cg-err bg-default hover:bg-cg-err-tint active:bg-cg-err-tint ${FOCUS} focus-visible:ring-1 focus-visible:ring-cg-err` },
        { color: 'warning', variant: 'outline', class: `ring-cg-warn text-cg-warn bg-default hover:bg-cg-warn-tint active:bg-cg-warn-tint ${FOCUS} focus-visible:ring-1 focus-visible:ring-cg-warn` },
        { color: 'error', variant: 'ghost', class: `text-cg-err hover:bg-cg-err-tint active:bg-cg-err-tint ${FOCUS}` },
        { color: 'warning', variant: 'ghost', class: `text-cg-warn hover:bg-cg-warn-tint active:bg-cg-warn-tint ${FOCUS}` },
        { size: 'xs', square: true, class: 'p-1 min-w-7' },
        { size: 'sm', square: true, class: 'p-1.5 min-w-8' },
        { size: 'md', square: true, class: 'p-2 min-w-9 min-h-9' },
        { size: 'lg', square: true, class: 'p-2.5 min-w-11' },
        { size: 'xl', square: true, class: 'p-3 min-w-12' }
      ],
      defaultVariants: { color: 'neutral', variant: 'outline' }
    },

    // A status: an 8px square and a word in its colour (soft, subtle). An
    // outline badge is a tag: a small --rule-strong frame, muted text.
    badge: {
      slots: {
        base: 'rounded-none font-medium whitespace-nowrap',
        leadingIcon: 'size-3.5'
      },
      variants: {
        size: {
          xs: { base: 'text-xs px-1 py-px gap-1' },
          sm: { base: 'text-xs px-1.5 py-px gap-1.5' },
          md: { base: 'text-[13px] px-1.5 py-0.5 gap-1.5' },
          lg: { base: 'text-sm px-2 py-0.5 gap-2' },
          xl: { base: 'text-sm px-2.5 py-1 gap-2' }
        }
      },
      compoundVariants: [
        { variant: SUBTLE, class: DOT },
        { color: 'success', variant: SUBTLE, class: 'text-cg-ok' },
        { color: 'warning', variant: SUBTLE, class: 'text-cg-warn' },
        { color: 'error', variant: SUBTLE, class: 'text-cg-err' },
        { color: ['primary', 'secondary', 'info'], variant: SUBTLE, class: 'text-cg-blue' },
        { color: 'neutral', variant: SUBTLE, class: 'text-muted before:bg-transparent before:ring-[1.5px] before:ring-inset before:ring-current' },
        { variant: 'outline', class: 'rounded-[2px] ring ring-inset' },
        { color: ['primary', 'neutral'], variant: 'outline', class: 'ring-accented text-muted' },
        { color: 'error', variant: 'outline', class: 'ring-cg-err text-cg-err' },
        { color: 'warning', variant: 'outline', class: 'ring-cg-warn text-cg-warn' },
        { color: 'success', variant: 'outline', class: 'ring-cg-ok text-cg-ok' },
        { color: ['secondary', 'info'], variant: 'outline', class: 'ring-cg-blue text-cg-blue' },
        { color: ['primary', 'neutral'], variant: 'solid', class: 'bg-inverted text-inverted' },
        { color: 'secondary', variant: 'solid', class: 'bg-cg-blue text-cg-paper' }
      ]
    },

    input: {
      slots: { base: R },
      variants: {
        size: {
          sm: { base: 'px-2.5 py-1.5 text-sm/5' },
          md: { base: 'px-3 py-2 text-[15px]/6' },
          lg: { base: 'px-3.5 py-2.5 text-base/6' }
        }
      },
      defaultVariants: BLUE
    },
    inputNumber: {
      slots: { base: R },
      variants: { size: { md: { base: 'px-3 py-2 text-[15px]/6' } } },
      defaultVariants: BLUE
    },
    inputTags: {
      slots: { root: R },
      defaultVariants: BLUE
    },
    textarea: {
      slots: { base: R },
      variants: { size: { md: { base: 'px-3 py-2 text-[15px]/6' } } },
      defaultVariants: BLUE
    },
    select: {
      slots: {
        base: R,
        content: POPOVER,
        item: 'before:rounded-[2px]'
      },
      variants: { size: { md: { base: 'px-3 py-2 text-[15px]/6', item: 'p-2 text-sm' } } },
      defaultVariants: BLUE
    },
    selectMenu: {
      slots: {
        base: R,
        content: POPOVER,
        item: 'before:rounded-[2px]'
      },
      variants: { size: { md: { base: 'px-3 py-2 text-[15px]/6', item: 'p-2 text-sm' } } },
      defaultVariants: BLUE
    },
    formField: {
      slots: {
        label: 'text-sm font-medium text-highlighted',
        hint: 'text-[13px] text-muted',
        help: 'mt-1.5 text-[13px] text-muted',
        description: 'text-[13px] text-muted',
        error: 'mt-1.5 text-[13px] text-error'
      }
    },
    checkbox: {
      slots: { base: 'rounded-[2px] ring-accented', label: 'font-normal' },
      defaultVariants: BLUE
    },
    checkboxGroup: {
      defaultVariants: BLUE
    },
    radioGroup: {
      slots: { label: 'font-normal' },
      variants: {
        variant: {
          card: { item: `${R} border-accented` }
        }
      },
      // A chosen tile: blue tint and a 2px blue frame, as the design's
      // type picker.
      compoundVariants: [
        { variant: 'card', color: 'secondary', class: { item: 'has-data-[state=checked]:border-cg-blue has-data-[state=checked]:bg-cg-blue-tint has-data-[state=checked]:shadow-[inset_0_0_0_1px_var(--cg-blue)]' } }
      ],
      defaultVariants: BLUE
    },
    switch: {
      slots: {
        base: 'rounded-[3px] data-[state=unchecked]:bg-(--cg-rule-strong)',
        thumb: 'rounded-[2px] shadow-none',
        label: 'font-normal'
      },
      defaultVariants: BLUE
    },
    progress: {
      slots: { base: 'rounded-none bg-accented', indicator: 'rounded-none' },
      defaultVariants: BLUE
    },
    link: {
      variants: {
        active: { true: 'text-highlighted' }
      }
    },

    dropdownMenu: {
      slots: {
        content: POPOVER,
        item: 'before:rounded-[2px]',
        label: 'text-[13px] font-semibold text-muted',
        separator: 'mx-0 bg-border'
      },
      variants: {
        active: {
          true: { item: 'text-inverted before:bg-inverted', itemLeadingIcon: 'text-inverted' },
          false: { item: MENU_ITEM }
        }
      }
    },
    commandPalette: {
      slots: {
        item: 'before:rounded-[2px]',
        label: 'text-[13px] font-semibold text-muted',
        itemLabelBase: 'text-highlighted [&>mark]:text-cg-blue [&>mark]:bg-transparent [&>mark]:underline'
      },
      variants: {
        active: {
          true: { item: 'text-highlighted before:bg-accented' },
          false: { item: 'text-default data-highlighted:not-data-disabled:text-highlighted data-highlighted:not-data-disabled:before:bg-accented' }
        }
      }
    },
    tooltip: {
      slots: {
        content: `${R} bg-inverted text-inverted ring-0 shadow-none text-[13px] h-auto py-1`
      }
    },
    popover: {
      slots: { content: POPOVER }
    },

    modal: {
      slots: {
        overlay: 'bg-(--cg-scrim)',
        content: 'divide-y divide-default',
        header: 'px-5 py-4 sm:px-6 min-h-14',
        body: 'p-5 sm:p-6',
        footer: 'px-5 py-4 sm:px-6',
        title: 'text-lg font-semibold text-highlighted',
        description: 'mt-1 text-muted text-sm'
      },
      variants: {
        fullscreen: {
          false: { content: `${R} ring ring-accented shadow-2xl` }
        }
      }
    },
    slideover: {
      slots: {
        overlay: 'bg-(--cg-scrim)',
        content: 'rounded-none sm:ring-accented sm:shadow-2xl',
        header: 'px-5 py-4 sm:px-6 min-h-14',
        title: 'text-lg font-semibold text-highlighted',
        description: 'mt-1 text-muted text-sm'
      }
    },
    toast: {
      slots: {
        root: `${R} ring ring-accented shadow-lg bg-default`,
        title: 'text-sm font-semibold text-highlighted',
        description: 'text-[13px] text-muted'
      }
    },
    toaster: {
      slots: { viewport: 'gap-2' }
    },
    // A banner: a full 1px frame in its colour, the tint behind, the icon in
    // the colour; never a thick bar on one side.
    alert: {
      slots: {
        root: 'rounded-none px-4 py-3 text-highlighted',
        title: 'text-sm font-semibold',
        description: 'text-sm text-default opacity-100',
        icon: 'mt-px'
      },
      compoundVariants: [
        { variant: ['soft', 'subtle', 'outline'], class: { root: 'ring ring-inset text-highlighted' } },
        { color: 'success', variant: ['soft', 'subtle', 'outline'], class: { root: 'bg-cg-ok-tint ring-cg-ok', icon: 'text-cg-ok' } },
        { color: 'warning', variant: ['soft', 'subtle', 'outline'], class: { root: 'bg-cg-warn-tint ring-cg-warn', icon: 'text-cg-warn' } },
        { color: 'error', variant: ['soft', 'subtle', 'outline'], class: { root: 'bg-cg-err-tint ring-cg-err', icon: 'text-cg-err' } },
        { color: ['primary', 'info', 'secondary'], variant: ['soft', 'subtle', 'outline'], class: { root: 'bg-cg-blue-tint ring-cg-blue', icon: 'text-cg-blue' } },
        { color: 'neutral', variant: ['soft', 'subtle', 'outline'], class: { root: 'bg-elevated ring-default', icon: 'text-muted' } }
      ]
    },

    // Ruled tables: the head on paper-2, 1px rules, 12/16 padding.
    table: {
      slots: {
        root: 'relative overflow-auto',
        thead: 'relative',
        tbody: 'isolate divide-y divide-default [&>tr]:transition-colors [&>tr]:hover:bg-elevated',
        th: 'px-3 py-2.5 first:ps-4 sm:first:ps-5 last:pe-4 sm:last:pe-5 text-[13px] font-semibold text-muted text-start bg-elevated border-b border-default whitespace-nowrap',
        td: 'px-3 py-3 first:ps-4 sm:first:ps-5 last:pe-4 sm:last:pe-5 text-sm text-default whitespace-nowrap',
        separator: 'hidden',
        empty: 'py-8 text-center text-sm text-muted'
      }
    },
    tabs: {
      slots: {
        list: 'rounded-none',
        trigger: 'rounded-none font-medium data-[state=inactive]:text-default',
        leadingIcon: 'size-4',
        indicator: 'rounded-none'
      },
      variants: {
        size: {
          md: { trigger: 'px-3.5 py-2.5 text-[15px] gap-2' }
        }
      },
      compoundVariants: [
        // mb-0: Nuxt UI pulls the list's divider 1px out of its box
        // (-mb-px), where the side-scrolling wrapper of a tab bar clips it.
        { variant: 'link', orientation: 'horizontal', class: { list: 'border-b border-default p-0 mb-0', indicator: '-bottom-px h-0.5 bg-inverted', trigger: 'data-[state=active]:text-highlighted data-[state=active]:font-semibold hover:bg-elevated' } },
        { variant: 'pill', class: { list: `p-0 bg-transparent ring ring-inset ring-accented gap-0 ${R}`, indicator: `inset-y-0 bg-inverted shadow-none ${R}`, trigger: 'data-[state=active]:text-inverted' } }
      ],
      defaultVariants: { variant: 'link', color: 'neutral' }
    },
    // A page's title block: the page's name, one line about it, the
    // primary action on the right.
    pageHeader: {
      slots: {
        root: 'border-b-0 py-0',
        headline: 'mb-1.5 text-sm font-normal text-muted',
        wrapper: 'flex-row flex-wrap items-end justify-between gap-x-6 gap-y-3',
        title: 'min-w-0 text-[1.375rem] sm:text-[1.625rem] leading-tight font-semibold tracking-[-0.015em] text-highlighted [overflow-wrap:anywhere] flex flex-wrap items-center gap-x-3 gap-y-2',
        description: 'mt-1 text-[0.9375rem] text-muted max-w-[72ch]',
        links: 'flex flex-wrap items-center gap-2'
      },
      variants: {
        title: { true: { description: 'mt-1' } }
      }
    },
    // The site tab bar: the active tab underlined in ink, as the design's
    // tabs.
    navigationMenu: {
      slots: {
        link: 'before:rounded-[2px] gap-2 text-[15px] whitespace-nowrap',
        linkLeadingIcon: 'size-4',
        linkLabel: 'overflow-visible'
      },
      compoundVariants: [
        { orientation: 'horizontal', highlight: true, class: { link: 'after:h-0.5 after:rounded-none after:inset-x-2' } },
        { orientation: 'horizontal', active: true, variant: 'link', class: { link: 'font-semibold' } }
      ],
      defaultVariants: { color: 'neutral' }
    },
    breadcrumb: {
      slots: {
        link: 'rounded-none text-sm font-medium text-muted',
        linkLeadingIcon: 'size-4',
        separatorIcon: 'size-4 text-dimmed'
      },
      compoundVariants: [
        { color: 'primary', active: true, class: { link: 'text-highlighted' } }
      ]
    },

    pageCard: {
      // A section of a page is a panel (PanelSection): square, a 1px rule
      // around it, a ruled header strip. The container stays a column at
      // every width (as a grid, from lg up, the card stretched every
      // button to its full width).
      slots: {
        root: 'rounded-none min-w-0',
        container: 'flex flex-col lg:flex min-w-0 gap-y-4 p-4 sm:p-5',
        wrapper: 'flex-none items-stretch min-w-0',
        header: 'mb-0',
        leadingIcon: 'size-5 text-cg-blue',
        title: 'text-base font-semibold leading-snug text-highlighted',
        description: 'mt-1 text-[13px] text-muted'
      },
      variants: {
        variant: {
          outline: { root: 'bg-default ring ring-default', description: 'text-muted' },
          subtle: { root: 'bg-elevated ring ring-default', description: 'text-muted' },
          soft: { root: 'bg-elevated', description: 'text-muted' }
        }
      }
    },
    card: {
      slots: { root: 'rounded-none' },
      variants: {
        variant: {
          outline: { root: 'bg-default ring ring-default' },
          subtle: { root: 'bg-default ring ring-default' },
          soft: { root: 'bg-elevated' }
        }
      }
    },
    empty: {
      // Inside a panel: set left, as the design's empty state; the icon in
      // blue, one sentence, the one action. Every size reads the same.
      slots: {
        root: 'rounded-none items-start text-start gap-4 px-4 py-6 sm:px-6 sm:py-7 lg:px-6 lg:py-7',
        header: 'items-start text-start max-w-[52ch] gap-1.5',
        // The avatar's own neutral colour would draw the icon in paper
        // on paper.
        avatar: 'rounded-none bg-transparent mb-2 [&>[data-slot=icon]]:text-cg-blue',
        title: 'font-semibold leading-snug text-highlighted',
        description: 'text-pretty text-start text-default',
        body: 'items-start',
        actions: 'justify-start items-center gap-x-5 gap-y-3',
        footer: 'items-start'
      },
      variants: {
        size: {
          xs: EMPTY, sm: EMPTY, md: EMPTY, lg: EMPTY, xl: EMPTY
        },
        variant: {
          outline: { root: 'bg-transparent ring-0' },
          subtle: { root: 'bg-transparent ring-0' },
          soft: { root: 'bg-transparent' }
        }
      }
    },
    avatar: {
      slots: { root: 'rounded-[3px]', fallback: 'font-medium' },
      variants: {
        color: {
          neutral: { root: 'bg-inverted', fallback: 'text-inverted', icon: 'text-inverted' }
        }
      }
    },
    kbd: {
      base: `${R} font-mono`
    },
    chip: {
      slots: { base: 'rounded-none' }
    },
    skeleton: {
      base: 'rounded-none bg-accented'
    },
    separator: {
      slots: { border: 'border-default' }
    },

    // A page's body holds one UContainer (the centred content column):
    // the vertical rhythm is the body's, the width the container's.
    dashboardPanel: {
      slots: {
        root: 'lg:not-last:border-e-0',
        body: 'flex flex-col gap-0 sm:gap-0 flex-1 overflow-y-auto p-0 sm:p-0 pt-5 pb-10 sm:pt-7 sm:pb-12 *:shrink-0'
      }
    },
    // The top bar: the same height as the sidebar's header
    // (--ui-header-height).
    dashboardNavbar: {
      slots: {
        root: 'bg-default px-3 sm:px-6 lg:px-8 gap-3',
        left: 'gap-2 min-w-0 flex-1',
        right: 'gap-1'
      }
    },
    dashboardSidebar: {
      slots: {
        root: 'bg-elevated',
        header: 'px-5 sm:px-5',
        body: 'gap-0 p-0 sm:px-0',
        footer: 'p-0 sm:px-0'
      },
      variants: {
        menu: { true: { header: 'sm:px-0', body: 'sm:px-0', footer: 'sm:px-0' } }
      }
    }
  }
})
