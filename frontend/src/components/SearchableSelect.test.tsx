// Tests for the SearchableSelect combobox: filtering, keyboard navigation,
// and selection via Enter/click.

import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import SearchableSelect, { type SearchableOption } from "./SearchableSelect";

const options = (): SearchableOption[] => [
  { value: "a", label: "Alpha", search: "alpha" },
  { value: "b", label: "Bravo", search: "bravo" },
  { value: "c", label: "Charlie", search: "charlie" },
];

function setup(initial = "a") {
  const onChange = vi.fn();
  const onOpen = vi.fn();
  render(() => (
    <SearchableSelect
      ariaLabel="Pick"
      value={initial}
      options={options}
      placeholder="Search…"
      emptyOption={{ value: "", label: "Default" }}
      onChange={onChange}
      onOpen={onOpen}
      data-testid="pick"
    />
  ));
  return { onChange, onOpen };
}

describe("SearchableSelect", () => {
  it("shows the selected label and opens on click", async () => {
    const user = userEvent.setup();
    setup();
    const trigger = screen.getByRole("button", { name: "Pick" });
    expect(trigger).toHaveTextContent("Alpha");
    await user.click(trigger);
    expect(screen.getByRole("combobox", { name: "Pick" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Alpha" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Default" })).toBeInTheDocument();
  });

  it("calls onOpen when the popup opens", async () => {
    const user = userEvent.setup();
    const { onOpen } = setup();
    await user.click(screen.getByRole("button", { name: "Pick" }));
    expect(onOpen).toHaveBeenCalledOnce();
  });

  it("navigates with ArrowDown/Up and selects with Enter", async () => {
    const user = userEvent.setup();
    const { onChange } = setup();
    const trigger = screen.getByRole("button", { name: "Pick" });
    trigger.focus();
    await user.keyboard("{Enter}");
    const input = screen.getByRole("combobox", { name: "Pick" });
    input.focus();
    await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    expect(onChange).toHaveBeenCalledWith("c");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  });

  it("filters options as the user types", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(screen.getByRole("button", { name: "Pick" }));
    const input = screen.getByRole("combobox", { name: "Pick" });
    await user.type(input, "bra");
    expect(screen.queryByRole("option", { name: "Alpha" })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Bravo" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Default" })).not.toBeInTheDocument();
  });

  it("selects the first match on Enter after filtering", async () => {
    const user = userEvent.setup();
    const { onChange } = setup();
    await user.click(screen.getByRole("button", { name: "Pick" }));
    const input = screen.getByRole("combobox", { name: "Pick" });
    await user.type(input, "char");
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenCalledWith("c");
  });

  it("closes on Escape without changing the value", async () => {
    const user = userEvent.setup();
    const { onChange } = setup();
    await user.click(screen.getByRole("button", { name: "Pick" }));
    await user.keyboard("{Escape}");
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  });

  it("clicks an option to select it", async () => {
    const user = userEvent.setup();
    const { onChange } = setup();
    await user.click(screen.getByRole("button", { name: "Pick" }));
    await user.click(screen.getByRole("option", { name: "Bravo" }));
    expect(onChange).toHaveBeenCalledWith("b");
  });
});
