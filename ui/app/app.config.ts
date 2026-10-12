/*
 * The panel design (ui/design/reference/*.dc.html) mapped onto Nuxt UI.
 *
 * Square corners everywhere (--ui-radius is 0 in main.css), hairlines for
 * structure, ink for primary actions and selection, blue as the one accent,
 * Geist Mono HUD labels for anything that names a thing rather than says it.
 * "primary" is the ink: a solid primary button is ink on paper and turns
 * blue under the pointer, as the artboards' .btn does.
 *
 * Classes here are spelled out (not the `hud` utility from main.css) so that
 * tailwind-merge can see which defaults they replace.
 */
const HUD = 'font-mono text-[11px] leading-[1.35] uppercase tracking-[0.14em] font-medium'
const INK_SOLID = 'text-inverted bg-inverted hover:bg-cg-blue hover:text-(--cg-on-blue) active:bg-cg-blue disabled:bg-inverted aria-disabled:bg-inverted disabled:text-inverted outline-cg-blue focus-visible:outline-2 focus-visible:outline-offset-2'
const INK_LINE = 'ring ring-inset ring-(--cg-ink) text-highlighted bg-transparent hover:bg-inverted hover:text-inverted active:bg-inverted active:text-inverted disabled:bg-transparent aria-disabled:bg-transparent disabled:text-highlighted outline-cg-blue focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:ring-(--cg-ink)'
const MENU_ITEM = 'text-default data-highlighted:text-highlighted data-[state=open]:text-highlighted data-highlighted:before:bg-accented data-[state=open]:before:bg-accented before:rounded-none'
// An empty state's sizes, all one (the empty config below).
const EMPTY = { avatar: 'size-8 text-[28px]', title: 'text-xl', description: 'text-sm' }
const POPOVER = 'rounded-none bg-default ring ring-(--cg-ink) shadow-[0_18px_40px_-24px_rgb(0_0_0_/_0.45)]'

export default defineAppConfig({
  ui: {
    colors: {
      primary: 'stratos',
      secondary: 'terra',
      neutral: 'basalt'
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
        base: `rounded-none ${HUD} justify-center transition-colors`
      },
      variants: {
        size: {
          xs: { base: 'px-2.5 py-1.5 text-[11px] gap-1.5 min-h-7' },
          sm: { base: 'px-3 py-2 text-[11px] gap-1.5 min-h-8' },
          md: { base: 'px-4 py-2.5 text-[11px] gap-2 min-h-10' },
          lg: { base: 'px-5 py-3 text-[11px] gap-2 min-h-11' },
          xl: { base: 'px-6 py-3.5 text-xs gap-2.5 min-h-12' }
        }
      },
      compoundVariants: [
        { color: ['primary', 'neutral'], variant: 'solid', class: INK_SOLID },
        { color: ['primary', 'neutral'], variant: 'outline', class: INK_LINE },
        { color: ['primary', 'neutral'], variant: ['soft', 'subtle'], class: 'ring-0 text-highlighted bg-accented hover:bg-inverted hover:text-inverted active:bg-inverted disabled:bg-accented aria-disabled:bg-accented' },
        { color: ['primary', 'neutral'], variant: 'ghost', class: 'text-default hover:bg-accented active:bg-accented outline-cg-blue focus-visible:outline-2 focus-visible:-outline-offset-2' },
        { color: ['primary', 'neutral'], variant: 'link', class: 'px-0 text-cg-blue hover:text-cg-blue hover:underline underline-offset-4 active:text-cg-blue disabled:text-cg-blue' },
        { color: 'secondary', variant: 'solid', class: 'bg-cg-blue text-(--cg-on-blue) hover:bg-inverted hover:text-inverted' },
        { color: ['success', 'warning', 'error', 'info', 'secondary'], variant: ['soft', 'subtle'], class: 'ring-0' },
        { color: 'success', variant: ['soft', 'subtle'], class: 'bg-cg-ok-bg text-cg-ok hover:bg-cg-ok hover:text-inverted' },
        { color: 'warning', variant: ['soft', 'subtle'], class: 'bg-cg-warn-bg text-cg-warn hover:bg-cg-warn hover:text-inverted' },
        { color: 'error', variant: ['soft', 'subtle'], class: 'bg-cg-err-bg text-cg-err hover:bg-cg-err hover:text-inverted' },
        { color: 'error', variant: 'outline', class: 'ring-cg-err hover:bg-cg-err hover:text-inverted' },
        { color: 'warning', variant: 'outline', class: 'ring-cg-warn hover:bg-cg-warn hover:text-inverted' },
        { size: 'xs', square: true, class: 'p-1.5 min-w-7' },
        { size: 'sm', square: true, class: 'p-2 min-w-8' },
        { size: 'md', square: true, class: 'p-2.5 min-w-10' },
        { size: 'lg', square: true, class: 'p-3 min-w-11' },
        { size: 'xl', square: true, class: 'p-3.5 min-w-12' }
      ]
    },

    // Status chips: mono, uppercase, a tint behind — always with words.
    badge: {
      slots: {
        base: `rounded-none ${HUD} whitespace-nowrap`
      },
      variants: {
        size: {
          xs: { base: 'text-[10px] px-1 py-px gap-1' },
          sm: { base: 'text-[11px] px-1.5 py-0.5 gap-1' },
          md: { base: 'text-[11px] px-[7px] py-[3px] gap-1' },
          lg: { base: 'text-[11px] px-2 py-1 gap-1.5' },
          xl: { base: 'text-xs px-2.5 py-1 gap-1.5' }
        }
      },
      compoundVariants: [
        { variant: ['soft', 'subtle'], class: 'ring-0' },
        { color: 'success', variant: ['soft', 'subtle'], class: 'bg-cg-ok-bg text-cg-ok' },
        { color: 'warning', variant: ['soft', 'subtle'], class: 'bg-cg-warn-bg text-cg-warn' },
        { color: 'error', variant: ['soft', 'subtle'], class: 'bg-cg-err-bg text-cg-err' },
        { color: 'neutral', variant: ['soft', 'subtle'], class: 'bg-accented text-muted' },
        { color: 'primary', variant: ['soft', 'subtle'], class: 'bg-accented text-highlighted' },
        { color: ['secondary', 'info'], variant: ['soft', 'subtle'], class: 'bg-transparent text-cg-blue ring ring-inset ring-cg-blue' },
        { color: ['primary', 'neutral'], variant: 'solid', class: 'bg-inverted text-inverted' },
        { color: ['primary', 'neutral'], variant: 'outline', class: 'ring-(--cg-line-strong) text-muted' },
        { color: 'secondary', variant: 'solid', class: 'bg-cg-blue text-(--cg-on-blue)' }
      ]
    },

    input: {
      slots: { base: 'rounded-none' },
      variants: {
        size: {
          sm: { base: 'px-2.5 py-2 text-sm/4' },
          md: { base: 'px-3 py-2.5 text-[15px]/5' },
          lg: { base: 'px-3.5 py-3 text-base/5' }
        }
      }
    },
    inputNumber: {
      slots: { base: 'rounded-none' },
      variants: { size: { md: { base: 'px-3 py-2.5 text-[15px]/5' } } }
    },
    textarea: {
      slots: { base: 'rounded-none' },
      variants: { size: { md: { base: 'px-3 py-2.5 text-[15px]/5' } } }
    },
    select: {
      slots: {
        base: 'rounded-none',
        content: POPOVER,
        item: `before:rounded-none`
      },
      variants: { size: { md: { base: 'px-3 py-2.5 text-[15px]/5', item: 'p-2 text-sm' } } }
    },
    selectMenu: {
      slots: {
        base: 'rounded-none',
        content: POPOVER,
        item: 'before:rounded-none'
      },
      variants: { size: { md: { base: 'px-3 py-2.5 text-[15px]/5', item: 'p-2 text-sm' } } }
    },
    formField: {
      slots: {
        label: `${HUD} text-muted`,
        hint: 'font-mono text-xs text-dimmed',
        help: 'mt-1.5 text-[13px] text-muted',
        description: 'text-[13px] text-muted',
        error: 'mt-1.5 text-[13px] text-error'
      }
    },
    checkbox: {
      slots: { base: 'rounded-none ring-(--cg-line-strong)', label: 'font-normal' }
    },
    radioGroup: {
      slots: { label: 'font-normal' }
    },
    switch: {
      slots: {
        base: 'rounded-none data-[state=unchecked]:bg-(--cg-line-strong)',
        thumb: 'rounded-none shadow-none',
        label: 'font-normal'
      }
    },

    dropdownMenu: {
      slots: {
        content: POPOVER,
        item: 'before:rounded-none',
        label: `${HUD} text-dimmed`,
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
        item: 'before:rounded-none',
        label: `${HUD} text-dimmed`,
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
        content: 'rounded-none bg-inverted text-inverted ring-0 shadow-none font-mono text-[11px] tracking-[0.04em] h-auto py-1'
      }
    },
    popover: {
      slots: { content: POPOVER }
    },

    modal: {
      slots: {
        overlay: 'bg-black/45 dark:bg-black/65',
        content: 'divide-y divide-default',
        header: 'px-5 py-4 sm:px-6 min-h-14',
        body: 'p-5 sm:p-6',
        footer: 'px-5 py-4 sm:px-6',
        title: 'disp text-xl text-highlighted',
        description: 'mt-1 text-muted text-sm'
      },
      variants: {
        fullscreen: {
          false: { content: 'rounded-none shadow-[0_30px_80px_-30px_rgb(0_0_0_/_0.5)] ring ring-(--cg-ink)' }
        }
      }
    },
    slideover: {
      slots: {
        overlay: 'bg-black/45 dark:bg-black/65',
        content: 'rounded-none sm:ring-(--cg-ink) sm:shadow-[0_30px_80px_-30px_rgb(0_0_0_/_0.5)]',
        header: 'px-5 py-4 sm:px-6 min-h-14',
        title: 'disp text-xl text-highlighted',
        description: 'mt-1 text-muted text-sm'
      }
    },
    toast: {
      slots: {
        root: 'rounded-none ring ring-(--cg-ink) shadow-[0_18px_40px_-20px_rgb(0_0_0_/_0.45)] bg-default',
        title: 'text-sm font-semibold text-highlighted',
        description: 'text-[13px] text-muted'
      }
    },
    toaster: {
      slots: { viewport: 'gap-2' }
    },
    alert: {
      slots: {
        root: 'rounded-none',
        title: 'text-sm font-semibold',
        description: 'text-[13px] opacity-100'
      },
      compoundVariants: [
        { variant: ['soft', 'subtle', 'outline'], class: { root: 'ring-0' } },
        { color: 'success', variant: ['soft', 'subtle'], class: { root: 'bg-cg-ok-bg text-cg-ok' } },
        { color: 'warning', variant: ['soft', 'subtle'], class: { root: 'bg-cg-warn-bg text-cg-warn' } },
        { color: 'error', variant: ['soft', 'subtle'], class: { root: 'bg-cg-err-bg text-cg-err' } },
        { color: ['primary', 'neutral', 'info', 'secondary'], variant: ['soft', 'subtle'], class: { root: 'bg-elevated text-highlighted border-l-2 border-cg-blue' } }
      ]
    },

    table: {
      slots: {
        root: 'relative overflow-auto',
        thead: 'relative',
        tbody: 'isolate divide-y divide-default [&>tr]:transition-colors [&>tr]:hover:bg-elevated',
        // A table sits flush in a section's card: its first and last
        // columns line up with the card's header.
        th: `px-2.5 py-2.5 first:ps-4 sm:first:ps-5 last:pe-4 sm:last:pe-5 ${HUD} text-muted text-start border-b border-default`,
        td: 'px-2.5 py-3 first:ps-4 sm:first:ps-5 last:pe-4 sm:last:pe-5 text-sm text-default whitespace-nowrap',
        separator: 'hidden',
        empty: `py-8 text-center ${HUD} text-muted`
      }
    },
    tabs: {
      slots: {
        list: 'rounded-none',
        trigger: 'rounded-none font-medium data-[state=inactive]:text-muted',
        leadingIcon: 'size-4',
        indicator: 'rounded-none'
      },
      variants: {
        size: {
          md: { trigger: 'px-3 py-3 text-sm gap-2' }
        }
      },
      compoundVariants: [
        // mb-0: Nuxt UI pulls the list's divider 1px out of its box
        // (-mb-px), where the side-scrolling wrapper of a tab bar clips it.
        { variant: 'link', orientation: 'horizontal', class: { list: 'border-b border-default p-0 mb-0', indicator: '-bottom-px h-0.5 bg-cg-blue', trigger: 'data-[state=active]:text-highlighted' } },
        { variant: 'pill', class: { list: 'p-0 bg-transparent ring ring-inset ring-default gap-0', indicator: 'inset-y-0 rounded-none bg-inverted shadow-none', trigger: 'data-[state=active]:text-inverted' } }
      ],
      defaultVariants: { variant: 'link' }
    },
    // A page's title block: the page's name, one line about it, the
    // primary action on the right.
    pageHeader: {
      slots: {
        root: 'border-b-0 py-0',
        headline: 'mb-1.5 text-sm font-normal text-muted',
        wrapper: 'flex-row flex-wrap items-center lg:items-center justify-between gap-x-6 gap-y-3',
        title: 'disp min-w-0 text-[24px] sm:text-[28px] leading-tight font-[560] text-highlighted [overflow-wrap:anywhere] flex flex-wrap items-center gap-x-3 gap-y-2',
        description: 'mt-1.5 text-sm text-muted max-w-[72ch]',
        links: 'flex flex-wrap items-center gap-2'
      },
      variants: {
        title: { true: { description: 'mt-1.5' } }
      }
    },
    // The site tab bar: icon and label, the active tab underlined in blue.
    navigationMenu: {
      slots: {
        link: 'before:rounded-none gap-2 text-sm whitespace-nowrap',
        linkLeadingIcon: 'size-4',
        linkLabel: 'overflow-visible'
      },
      compoundVariants: [
        { orientation: 'horizontal', highlight: true, class: { link: 'after:h-0.5 after:rounded-none after:inset-x-2' } }
      ]
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
      // A section of a page is a card (PanelSection): square, on a
      // hairline. The container stays a column at every width (as a grid,
      // from lg up, the card stretched every button to its full width).
      slots: {
        root: 'rounded-none min-w-0',
        container: 'flex flex-col lg:flex min-w-0 gap-y-4 p-4 sm:p-5',
        wrapper: 'flex-none items-stretch min-w-0',
        header: 'mb-0',
        leadingIcon: 'size-5 text-cg-blue',
        title: 'disp [font-stretch:108%] text-lg leading-snug text-highlighted',
        description: 'mt-1 text-sm text-muted'
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
      // Inside a section's card: centred, one sentence, the one action.
      // Every size reads the same (one empty state across the panel): the
      // icon in blue just above a display title, close to the card's top.
      slots: {
        root: 'rounded-none gap-4 px-4 py-6 sm:px-6 sm:py-7 lg:px-8 lg:py-7',
        header: 'max-w-md gap-1.5',
        // The avatar's own neutral colour would draw the icon in paper
        // on paper.
        avatar: 'rounded-none bg-transparent mb-1 [&>[data-slot=icon]]:text-cg-blue',
        title: 'disp [font-stretch:108%] leading-snug text-highlighted',
        description: 'text-pretty text-muted'
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
      slots: { root: 'rounded-none', fallback: 'font-mono font-medium' },
      variants: {
        color: {
          neutral: { root: 'bg-inverted', fallback: 'text-inverted', icon: 'text-inverted' }
        }
      }
    },
    kbd: {
      base: 'rounded-none font-mono'
    },
    chip: {
      slots: { base: 'rounded-none' }
    },
    progress: {
      slots: { base: 'rounded-none bg-accented', indicator: 'rounded-none' },
      variants: { color: { primary: { indicator: 'bg-cg-blue' } } }
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
        body: 'flex flex-col gap-0 sm:gap-0 flex-1 overflow-y-auto p-0 sm:p-0 pt-6 pb-12 sm:pt-8 sm:pb-16 *:shrink-0'
      }
    },
    // The top bar: the same height as the sidebar's header
    // (--ui-header-height), so their bottom lines are one line.
    dashboardNavbar: {
      slots: {
        root: 'bg-default px-4 sm:px-6 lg:px-8 gap-3',
        left: 'gap-3 min-w-0 flex-1',
        right: 'gap-1'
      }
    },
    dashboardSidebar: {
      slots: {
        root: 'bg-elevated',
        header: 'px-5 sm:px-5 border-b border-default',
        body: 'gap-0 p-0 sm:px-0',
        footer: 'p-0 sm:px-0'
      },
      variants: {
        menu: { true: { header: 'sm:px-0', body: 'sm:px-0', footer: 'sm:px-0' } }
      }
    }
  }
})
