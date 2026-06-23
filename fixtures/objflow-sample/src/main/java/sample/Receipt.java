package sample;

public class Receipt {
    private String code;
    private String label;
    private String tier;

    public Receipt() { }
    public Receipt(String code) { this.code = code; }

    public void setCode(String c) { this.code = c; }
    public void setLabel(String l) { this.label = l; }
    public void setTier(String t) { this.tier = t; }

    public String getCode() { return code; }
    public String getLabel() { return label; }
    public String getTier() { return tier; }
}
